package location_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
)

// scriptedProvider returns its scripted results in order, then fails.
type scriptedProvider struct {
	locs  []location.Location
	errs  []error
	calls atomic.Int32
}

func (p *scriptedProvider) Lookup(context.Context) (location.Location, error) {
	index := int(p.calls.Add(1)) - 1
	if index < len(p.errs) && p.errs[index] != nil {
		return location.Location{}, p.errs[index]
	}
	if index < len(p.locs) {
		return p.locs[index], nil
	}
	return location.Location{}, errors.New("no scripted lookup left")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var (
	london = location.Location{City: "London", Region: "England", Timezone: "Europe/London"}
	paris  = location.Location{City: "Paris", Region: "Ile-de-France", Timezone: "Europe/Paris"}
)

// TestLocationFallsBackToLastThenConfigThenNone is the fallback chain from
// issue #23: failed lookup -> last known value -> config -> no location.
func TestLocationFallsBackToLastThenConfigThenNone(t *testing.T) {
	// A success is remembered, and a later failure keeps it.
	provider := &scriptedProvider{
		locs: []location.Location{london},
		errs: []error{nil, errors.New("lookup down")},
	}
	service := location.NewService(provider, paris, 24*time.Hour, discardLogger())
	if got, ok := service.Current(); !ok || got != paris {
		t.Fatalf("Current before lookup = %+v/%v, want config fallback", got, ok)
	}
	service.Refresh(context.Background())
	if got, ok := service.Current(); !ok || got != london {
		t.Fatalf("Current after success = %+v/%v, want London", got, ok)
	}
	service.Refresh(context.Background())
	if got, ok := service.Current(); !ok || got != london {
		t.Fatalf("Current after failure = %+v/%v, want last known London", got, ok)
	}

	// No success yet: config is the fallback.
	provider = &scriptedProvider{errs: []error{errors.New("lookup down")}}
	service = location.NewService(provider, paris, 24*time.Hour, discardLogger())
	service.Refresh(context.Background())
	if got, ok := service.Current(); !ok || got != paris {
		t.Fatalf("Current after first failure = %+v/%v, want config fallback", got, ok)
	}

	// No success and no config: no location.
	provider = &scriptedProvider{errs: []error{errors.New("lookup down")}}
	service = location.NewService(provider, location.Location{}, 24*time.Hour, discardLogger())
	service.Refresh(context.Background())
	if got, ok := service.Current(); ok {
		t.Fatalf("Current = %+v, want no location", got)
	}
}

func TestLocationWatchRefreshesUntilCancelled(t *testing.T) {
	provider := &scriptedProvider{locs: []location.Location{london}}
	service := location.NewService(provider, location.Location{}, 10*time.Millisecond, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go service.Watch(ctx)

	deadline := time.Now().Add(time.Second)
	for provider.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls := provider.calls.Load(); calls < 2 {
		t.Fatalf("lookups = %d, want repeated refreshes", calls)
	}
}

func TestSystemPromptCarriesPlaceWithoutCoordinates(t *testing.T) {
	line := location.PromptLine(london)
	for _, want := range []string{"London", "England", "Europe/London"} {
		if !strings.Contains(line, want) {
			t.Errorf("prompt line %q is missing %q", line, want)
		}
	}
	for _, banned := range []string{"latitude", "longitude", "51.", "-0.1"} {
		if strings.Contains(line, banned) {
			t.Errorf("prompt line %q contains %q", line, banned)
		}
	}

	service := location.NewService(nil, london, 24*time.Hour, discardLogger())
	if got := service.SystemPrompt("Be brief."); !strings.Contains(got, "Be brief.") || !strings.Contains(got, "London") {
		t.Errorf("SystemPrompt = %q, want base prompt plus the place", got)
	}

	empty := location.NewService(nil, location.Location{}, 24*time.Hour, discardLogger())
	if got := empty.SystemPrompt("Be brief."); got != "Be brief." {
		t.Errorf("SystemPrompt = %q, want the base prompt unchanged", got)
	}
}

// TestIPWhoIsProviderKeepsOnlyCityRegionAndTimezone feeds the provider the
// documented response, coordinates included, and checks they do not survive.
func TestIPWhoIsProviderKeepsOnlyCityRegionAndTimezone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fields"); got != "" && got != "city,region,timezone" {
			t.Errorf("fields = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"success": true,
			"city": "London",
			"region": "England",
			"latitude": 51.5074,
			"longitude": -0.1278,
			"timezone": {"id": "Europe/London"}
		}`)
	}))
	defer server.Close()

	provider := location.NewIPWhoIs(server.Client(), server.URL)
	got, err := provider.Lookup(context.Background())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != london {
		t.Fatalf("Lookup = %+v, want %+v", got, london)
	}
	rendered := fmt.Sprintf("%+v", got)
	for _, banned := range []string{"51.5074", "-0.1278", "latitude", "longitude"} {
		if strings.Contains(rendered, banned) {
			t.Errorf("provider retained %q: %s", banned, rendered)
		}
	}
}

// TestIPWhoIsProviderAcceptsAFieldsResponseWithoutSuccess covers the real
// response shape: a `fields=` request omits every field that was not asked
// for, including success, and that must still count as a lookup.
func TestIPWhoIsProviderAcceptsAFieldsResponseWithoutSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"region":"Maryland","city":"Hagerstown",
			"timezone":{"id":"America/New_York","abbr":"EST"}}`)
	}))
	defer server.Close()

	provider := location.NewIPWhoIs(server.Client(), server.URL)
	got, err := provider.Lookup(context.Background())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := location.Location{City: "Hagerstown", Region: "Maryland", Timezone: "America/New_York"}
	if got != want {
		t.Fatalf("Lookup = %+v, want %+v", got, want)
	}
}

func TestIPWhoIsProviderReportsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success": false, "message": "Rate limit exceeded"}`)
	}))
	defer server.Close()

	provider := location.NewIPWhoIs(server.Client(), server.URL)
	if _, err := provider.Lookup(context.Background()); err == nil {
		t.Fatal("Lookup succeeded, want an error")
	}
}
