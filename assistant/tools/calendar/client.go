package calendar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
)

const (
	defaultFetchTimeout = 10 * time.Second
	defaultMaxBodyBytes = 2 << 20 // 2 MiB
	cacheTTL            = 10 * time.Minute
)

// HealthInfo reports a snapshot of the calendar feed's status.
type HealthInfo struct {
	LastFetch  time.Time
	EventCount int
	ErrorClass string
}

// Client fetches and parses the ICS feed behind an https-only check, body limit,
// redirect restrictions and a 10-minute cache with fallback to last good copy.
type Client struct {
	httpClient *http.Client
	logger     *slog.Logger
	now        func() time.Time

	mu           sync.Mutex
	cachedEvents []EventInstance
	cacheURL     string
	cacheZone    string
	cacheStart   time.Time
	cacheEnd     time.Time
	cachedAt     time.Time
	lastError    error
	errorClass   string
}

// newClient builds a calendar client with the standard 10 s timeout and security checks.
func newClient(logger *slog.Logger, now func() time.Time) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	httpClient := httpclient.New(httpclient.Options{
		Timeout: defaultFetchTimeout,
	})
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			if !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
				return errors.New("redirect to another host is not allowed")
			}
		}
		if err := httpclient.RequireSecureOrigin(req.URL.String(), false); err != nil {
			return err
		}
		return nil
	}

	return &Client{
		httpClient: httpClient,
		logger:     logger,
		now:        now,
	}
}

// clock returns the client's current time source.
func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Fetch retrieves and parses events within [windowStart, windowEnd). If cached
// events are fresh, it returns them without an HTTP round-trip. If a fetch fails
// but a previous good copy exists, it keeps and returns the cached copy while
// recording the error for Health.
func (c *Client) Fetch(ctx context.Context, rawURL string, loc *time.Location, windowStart, windowEnd time.Time) ([]EventInstance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Never reuse private events or stale fallback across a feed, zone, or window change.
	rawURL = strings.TrimSpace(rawURL)
	if loc == nil {
		loc = time.UTC
	}
	if c.cacheURL != rawURL || c.cacheZone != loc.String() || !c.cacheStart.Equal(windowStart) || !c.cacheEnd.Equal(windowEnd) {
		c.cachedEvents = nil
		c.cachedAt = time.Time{}
		c.cacheURL, c.cacheZone = rawURL, loc.String()
		c.cacheStart, c.cacheEnd = windowStart, windowEnd
	}

	now := c.clock()
	if c.cachedEvents != nil && now.Sub(c.cachedAt) < cacheTTL {
		return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
	}

	if rawURL == "" {
		c.errorClass = "not-configured"
		c.lastError = errors.New("CALENDAR_ICS_URL is not configured")
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}

	if err := httpclient.RequireSecureOrigin(rawURL, false); err != nil {
		c.errorClass = "insecure_origin"
		c.lastError = err
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		c.errorClass = "bad_url"
		c.lastError = errors.New("invalid calendar URL")
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}
	req.Header.Set("User-Agent", "clanka-bot/calendar")
	req.Header.Set("Accept", "text/calendar, text/plain;q=0.9, */*;q=0.8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.errorClass = classifyTransportError(err)
		c.lastError = sanitizeError(err)
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.errorClass = fmt.Sprintf("http_%d", resp.StatusCode)
		c.lastError = fmt.Errorf("upstream calendar returned status %d", resp.StatusCode)
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass, "status", resp.StatusCode)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}

	lr := io.LimitReader(resp.Body, int64(defaultMaxBodyBytes)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		c.errorClass = "read_error"
		c.lastError = errors.New("failed to read calendar response")
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}
	if len(data) > defaultMaxBodyBytes {
		c.errorClass = "size_limit"
		c.lastError = errors.New("calendar feed exceeded 2 MiB limit")
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}

	// Expand across a bounded window: today plus 7 days.
	events, err := parseICS(bytes.NewReader(data), loc, windowStart, windowEnd)
	if err != nil {
		c.errorClass = "parse_error"
		c.lastError = errors.New("failed to parse calendar feed")
		c.logger.Error("calendar fetch failed", "error_class", c.errorClass)
		if c.cachedEvents != nil {
			return c.filterEvents(c.cachedEvents, windowStart, windowEnd), nil
		}
		return nil, c.lastError
	}

	// A successful empty calendar is cached too.
	if events == nil {
		events = []EventInstance{}
	}
	c.cachedEvents = events
	c.cachedAt = now
	c.lastError = nil
	c.errorClass = ""

	return events, nil
}

// filterEvents returns all cached events overlapping [windowStart, windowEnd).
func (c *Client) filterEvents(events []EventInstance, windowStart, windowEnd time.Time) []EventInstance {
	var result []EventInstance
	for _, ev := range events {
		if ev.Start.Before(windowEnd) && ev.End.After(windowStart) {
			result = append(result, ev)
		}
	}
	return result
}

// HealthInfo returns the current cached health snapshot without making an HTTP request.
func (c *Client) HealthInfo() HealthInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return HealthInfo{
		LastFetch:  c.cachedAt,
		EventCount: len(c.cachedEvents),
		ErrorClass: c.errorClass,
	}
}

// LastError returns the error from the last fetch attempt, if any.
func (c *Client) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

// ErrorClass returns the categorized error class from the last fetch attempt.
func (c *Client) ErrorClass() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errorClass
}

// classifyTransportError groups network and HTTP errors into categorized classes without URLs.
func classifyTransportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if strings.Contains(urlErr.Error(), "redirect to another host is not allowed") {
			return "redirect_forbidden"
		}
		if strings.Contains(urlErr.Error(), "plain http would send the credential in clear text") {
			return "insecure_origin"
		}
		if urlErr.Timeout() {
			return "timeout"
		}
	}
	return "network"
}

// sanitizeError removes the URL from url.Error to guarantee secrets are never logged or returned.
func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if errors.Is(err, context.DeadlineExceeded) || (urlErr.Err != nil && urlErr.Timeout()) {
			return errors.New("calendar request timed out")
		}
		if urlErr.Err != nil {
			return fmt.Errorf("%s: %v", urlErr.Op, urlErr.Err)
		}
		return errors.New(urlErr.Op)
	}
	return err
}
