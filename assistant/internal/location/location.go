// Package location resolves roughly where the Pi is from its public IP and
// keeps only the place names the assistant may use. Coordinates are never
// modelled, so they cannot reach a prompt or a Tool result.
package location

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultProviderURL is the free IP geolocation service. The fields parameter
// asks it not to send coordinates at all; success is requested too because the
// service omits fields that are not asked for.
const DefaultProviderURL = "https://ipwho.is/?fields=success,city,region,timezone"

const maxResponseBytes = 64 << 10

// Retry schedule used while no lookup has ever succeeded, so a Pi that boots
// before Wi-Fi or DNS is ready recovers without a restart.
const (
	defaultRetryInitial = 30 * time.Second
	defaultRetryMax     = 15 * time.Minute
	// retryAfterSuccess spaces a failure that follows a known place. It keeps
	// the place fresh without hammering a flaky provider, and never exceeds the
	// configured refresh interval.
	retryAfterSuccess = time.Hour
)

// Location is the only place data the assistant keeps: city, region and
// timezone. There is deliberately no latitude or longitude field.
type Location struct {
	City     string
	Region   string
	Timezone string
}

// Known reports whether the location has anything worth putting in a prompt.
func (l Location) Known() bool {
	return strings.TrimSpace(l.City) != "" || strings.TrimSpace(l.Timezone) != ""
}

// Provider looks up the current location. The production adapter uses the
// public IP; tests inject a fake.
type Provider interface {
	Lookup(ctx context.Context) (Location, error)
}

// Clock supplies the timer and wall clock the retry loop uses. Production uses
// real time; tests inject a fake so a backoff schedule can be observed without
// sleeping.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Option configures a Service.
type Option func(*Service)

// WithClock replaces the timer and wall clock. Tests use it to advance the
// retry schedule deterministically.
func WithClock(clock Clock) Option {
	return func(s *Service) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// Service holds the last successful lookup and the fallback chain.
type Service struct {
	provider Provider
	fallback Location
	refresh  time.Duration
	logger   *slog.Logger
	clock    Clock

	mu          sync.Mutex
	last        Location
	succeeded   bool
	attempted   bool
	nextAttempt time.Time
}

// NewService builds the location service. A nil provider disables lookups and
// leaves the config fallback in place. logger may be nil.
func NewService(provider Provider, fallback Location, refresh time.Duration, logger *slog.Logger, opts ...Option) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		provider: provider,
		fallback: fallback,
		refresh:  refresh,
		logger:   logger,
		clock:    realClock{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Refresh performs one lookup. A failure leaves the last known value (or the
// config fallback) untouched.
func (s *Service) Refresh(ctx context.Context) {
	s.lookup(ctx)
}

// lookup performs one lookup and reports whether a known place was stored.
func (s *Service) lookup(ctx context.Context) bool {
	if s == nil || s.provider == nil {
		return false
	}
	loc, err := s.provider.Lookup(ctx)
	s.mu.Lock()
	s.attempted = true
	s.mu.Unlock()
	if err != nil {
		s.logger.Warn("location lookup failed", "error", err.Error())
		return false
	}
	if !loc.Known() {
		s.logger.Warn("location lookup returned no place")
		return false
	}
	s.mu.Lock()
	s.last = loc
	s.succeeded = true
	s.mu.Unlock()
	s.logger.Info("location updated", "city", loc.City, "region", loc.Region, "timezone", loc.Timezone)
	return true
}

// Watch refreshes every interval until ctx is done. Call Refresh once before
// serving so the first request already has a location. If that first lookup
// failed, Watch retries on a bounded backoff (30 s, 1 m, 2 m ... 15 m) until a
// lookup succeeds, then settles back onto the refresh interval.
func (s *Service) Watch(ctx context.Context) {
	if s == nil || s.provider == nil || s.refresh <= 0 {
		return
	}
	delay, backoff := s.firstDelay()
	s.scheduleNext(delay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(delay):
		}
		switch {
		case s.lookup(ctx):
			// A success resets the backoff and returns to the normal cadence.
			backoff = defaultRetryInitial
			delay = s.refresh
		case s.hasSucceeded():
			// A place is already known, so a blip only needs a gentle retry.
			delay = retryAfterSuccess
			if s.refresh < delay {
				delay = s.refresh
			}
		default:
			delay = backoff
			backoff = nextBackoff(backoff, defaultRetryMax)
		}
		s.scheduleNext(delay)
	}
}

// firstDelay picks the wait before the first Watch lookup. With no attempt yet
// it preserves the plain refresh cadence; after a failed boot lookup it starts
// the backoff schedule so the failed place is retried promptly.
func (s *Service) firstDelay() (delay, backoff time.Duration) {
	backoff = defaultRetryInitial
	if !s.hasAttempted() || s.hasSucceeded() {
		return s.refresh, backoff
	}
	return defaultRetryInitial, nextBackoff(defaultRetryInitial, defaultRetryMax)
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > max {
		return max
	}
	return next
}

// scheduleNext records when the next lookup is due so Health can say how long
// a retry will take.
func (s *Service) scheduleNext(delay time.Duration) {
	s.mu.Lock()
	s.nextAttempt = s.clock.Now().Add(delay)
	s.mu.Unlock()
}

func (s *Service) hasAttempted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempted
}

func (s *Service) hasSucceeded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.succeeded
}

// Health reports whether a place is known for prompts and the weather Tool.
// It reads the cached/fallback chain and makes no network call.
func (s *Service) Health(context.Context) error {
	if _, ok := s.Current(); ok {
		return nil
	}
	if s == nil {
		return errors.New("location: no place known")
	}
	s.mu.Lock()
	next := s.nextAttempt
	s.mu.Unlock()
	if next.IsZero() {
		return errors.New("location: no place known")
	}
	if remaining := next.Sub(s.clock.Now()); remaining > 0 {
		return errors.New("location: no place known yet; retrying in " + shortDuration(remaining))
	}
	return errors.New("location: no place known yet; retrying now")
}

// shortDuration renders a retry countdown for the Health line.
func shortDuration(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// Current returns the last successful lookup, then the config fallback, then
// no location at all.
func (s *Service) Current() (Location, bool) {
	if s == nil {
		return Location{}, false
	}
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	if last.Known() {
		return last, true
	}
	if s.fallback.Known() {
		return s.fallback, true
	}
	return Location{}, false
}

// SystemPrompt appends the place line to base. With no location it returns base
// unchanged, so the prompt builder's default still applies.
func (s *Service) SystemPrompt(base string) string {
	loc, ok := s.Current()
	if !ok {
		return base
	}
	line := PromptLine(loc)
	if strings.TrimSpace(base) == "" {
		return line
	}
	return strings.TrimSpace(base) + "\n\n" + line
}

// PromptLine renders the allowed place facts for the agent's system prompt.
func PromptLine(loc Location) string {
	city := strings.TrimSpace(loc.City)
	region := strings.TrimSpace(loc.Region)
	timezone := strings.TrimSpace(loc.Timezone)

	var place string
	switch {
	case city != "" && region != "":
		place = city + ", " + region
	case city != "":
		place = city
	case region != "":
		place = region
	}
	if place == "" && timezone == "" {
		return ""
	}
	if place == "" {
		return "The user's timezone is " + timezone + "."
	}
	if timezone == "" {
		return "The user is in " + place + "."
	}
	return "The user is in " + place + ". Their timezone is " + timezone + "."
}

// ipWhoIs is the public-IP adapter for ipwho.is. The response struct has no
// coordinate fields, so latitude and longitude are discarded during decoding.
type ipWhoIs struct {
	client *http.Client
	url    string
}

// NewIPWhoIs builds the ipwho.is provider. An empty url uses DefaultProviderURL.
func NewIPWhoIs(client *http.Client, url string) Provider {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if url == "" {
		url = DefaultProviderURL
	}
	return &ipWhoIs{client: client, url: url}
}

type ipWhoIsResponse struct {
	// Success is a pointer because ipwho.is omits fields that are not asked
	// for, so an absent success must not be read as failure.
	Success  *bool  `json:"success"`
	Message  string `json:"message"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Timezone struct {
		ID string `json:"id"`
	} `json:"timezone"`
}

// Lookup fetches the place for this machine's public IP.
func (p *ipWhoIs) Lookup(ctx context.Context) (Location, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return Location{}, fmt.Errorf("location: build request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Location{}, fmt.Errorf("location: lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Location{}, fmt.Errorf("location: lookup status %d", resp.StatusCode)
	}
	var body ipWhoIsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil {
		return Location{}, fmt.Errorf("location: decode response: %w", err)
	}
	if body.Success != nil && !*body.Success {
		if body.Message == "" {
			body.Message = "lookup unsuccessful"
		}
		return Location{}, errors.New("location: " + body.Message)
	}
	return Location{City: body.City, Region: body.Region, Timezone: body.Timezone.ID}, nil
}
