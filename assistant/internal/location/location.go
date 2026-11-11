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
// asks it not to send coordinates at all.
const DefaultProviderURL = "https://ipwho.is/?fields=city,region,timezone"

const maxResponseBytes = 64 << 10

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

// Service holds the last successful lookup and the fallback chain.
type Service struct {
	provider Provider
	fallback Location
	refresh  time.Duration
	logger   *slog.Logger

	mu   sync.Mutex
	last Location
}

// NewService builds the location service. A nil provider disables lookups and
// leaves the config fallback in place. logger may be nil.
func NewService(provider Provider, fallback Location, refresh time.Duration, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{provider: provider, fallback: fallback, refresh: refresh, logger: logger}
}

// Refresh performs one lookup. A failure leaves the last known value (or the
// config fallback) untouched.
func (s *Service) Refresh(ctx context.Context) {
	if s == nil || s.provider == nil {
		return
	}
	loc, err := s.provider.Lookup(ctx)
	if err != nil {
		s.logger.Warn("location lookup failed", "error", err.Error())
		return
	}
	if !loc.Known() {
		s.logger.Warn("location lookup returned no place")
		return
	}
	s.mu.Lock()
	s.last = loc
	s.mu.Unlock()
	s.logger.Info("location updated", "city", loc.City, "region", loc.Region, "timezone", loc.Timezone)
}

// Watch refreshes every interval until ctx is done. Call Refresh once before
// serving so the first request already has a location.
func (s *Service) Watch(ctx context.Context) {
	if s == nil || s.provider == nil || s.refresh <= 0 {
		return
	}
	ticker := time.NewTicker(s.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Refresh(ctx)
		}
	}
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
	Success  bool   `json:"success"`
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
	if !body.Success {
		if body.Message == "" {
			body.Message = "lookup unsuccessful"
		}
		return Location{}, errors.New("location: " + body.Message)
	}
	return Location{City: body.City, Region: body.Region, Timezone: body.Timezone.ID}, nil
}
