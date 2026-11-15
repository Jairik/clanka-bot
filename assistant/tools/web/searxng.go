package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxBodyBytes bounds a JSON page from SearXNG. Ten short results are a
	// few kilobytes; this leaves room while refusing a pathological response.
	maxBodyBytes = 512 << 10
	// searchPath and healthPath are SearXNG's documented endpoints.
	searchPath = "/search"
	healthPath = "/healthz"
	// userAgent identifies this internal caller. SearXNG needs no key.
	userAgent = "clanka-bot/1.0 (+https://github.com/Jairik/clanka-bot)"
)

// searxng is the Provider for a local SearXNG instance with JSON enabled.
type searxng struct {
	baseURL string
	// host is the log-safe origin host[:port]. It never carries userinfo, a
	// path or the query, so a transport error can name the upstream without
	// exposing the request URL.
	host   string
	client *http.Client
}

// newSearxng builds the SearXNG provider. A nil client uses the shared
// keep-alive constructor; tests may pass their own.
func newSearxng(baseURL string, timeout time.Duration, client *http.Client) *searxng {
	if client == nil {
		client = newKeepAliveClient(timeout)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &searxng{
		baseURL: baseURL,
		host:    endpointHost(baseURL),
		client:  client,
	}
}

// endpointHost reduces a base URL to the host[:port] a log line may name. A
// URL that does not parse yields no host rather than a guess.
func endpointHost(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// transportError is one bounded search failure. Its message names only the
// failure kind and the upstream host; the request URL carries q= and must
// never appear in a log line. The wrapped error is kept for errors.Is/As
// (timeouts and cancellation), not for printing.
type transportError struct {
	kind string
	host string
	err  error
}

// newTransportError bounds one transport failure. An empty kind is classified
// from the error, and a blank host still yields a useful message.
func newTransportError(host, kind string, err error) *transportError {
	if kind == "" {
		kind = transportKind(err)
	}
	return &transportError{kind: kind, host: host, err: err}
}

// Error is safe to log: only the kind and host, never the full URL.
func (e *transportError) Error() string {
	if e.host == "" {
		return "searxng: request failed (" + e.kind + ")"
	}
	return "searxng: request failed (" + e.kind + ") contacting " + e.host
}

// Unwrap exposes the original failure to errors.Is/As only.
func (e *transportError) Unwrap() error { return e.err }

// transportKind names the transport phase for a local log line. It mirrors the
// bounded vocabulary the GitHub Tools use and never includes an address.
func transportKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial":
			return "connect"
		case "remote error":
			return "tls"
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "other"
}

// searxngResponse is the documented JSON search payload. Only the three fields
// web.search returns are decoded, so a future SearXNG field cannot widen the
// Tool output by accident.
type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search asks SearXNG for JSON results and maps them to the short Tool shape.
// A transport error, non-2xx status, oversize body or malformed JSON is an
// error; an empty result list is a successful empty search.
func (s *searxng) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if maxResults <= 0 {
		maxResults = defaultResults
	}
	if maxResults > maxResultLimit {
		maxResults = maxResultLimit
	}
	endpoint, err := url.Parse(s.baseURL + searchPath)
	if err != nil {
		return nil, fmt.Errorf("searxng: build search URL: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("format", "json")
	// Web results only: images, videos and news would widen the prompt for
	// no benefit to spoken answers.
	params.Set("categories", "general")
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		// A build failure wraps a *url.Error whose text repeats the URL and
		// q=; return the same bounded shape as a transport failure.
		return nil, newTransportError(s.host, "request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		// client.Do returns a *url.Error that echoes the full request URL,
		// including the query; log only the kind and host.
		return nil, newTransportError(s.host, "", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("searxng: search returned %s", resp.Status)
	}
	// Read one byte past the cap so an oversize page is detected rather than
	// silently truncated into invalid JSON.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("searxng: read response: %w", err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("searxng: response exceeds %d bytes", maxBodyBytes)
	}
	var payload searxngResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("searxng: invalid JSON: %w", err)
	}

	results := make([]Result, 0, min(maxResults, len(payload.Results)))
	for _, item := range payload.Results {
		title := collapse(item.Title)
		link := strings.TrimSpace(item.URL)
		if title == "" || link == "" {
			continue
		}
		if !usableURL(link) {
			continue
		}
		// A link longer than the bound is more likely generated or
		// pathological than useful; never hand the model a truncated URL.
		if len([]rune(link)) > maxURLRunes {
			continue
		}
		results = append(results, Result{
			Title:   truncate(title, maxTitleRunes),
			URL:     link,
			Snippet: truncate(collapse(item.Content), maxSnippetRunes),
		})
		if len(results) == maxResults {
			break
		}
	}
	return results, nil
}

// Health checks SearXNG's documented /healthz endpoint.
func (s *searxng) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+healthPath, nil)
	if err != nil {
		return fmt.Errorf("searxng: build health request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("searxng: health request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("searxng: health returned %s", resp.Status)
	}
	return nil
}

// usableURL keeps only absolute HTTP(S) links. A relative or javascript: link
// is not useful to the Agent and must never be offered as a result.
func usableURL(link string) bool {
	parsed, err := url.Parse(link)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// collapse turns arbitrary result text into one short line: no newlines,
// repeated spaces or tabs for the Agent prompt to sort through.
func collapse(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
