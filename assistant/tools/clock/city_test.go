package clock_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func clockRegistry(t *testing.T, now func() time.Time, zone *tz.Service) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(2 * time.Second)
	if err := clock.NewModule().Register(registry, tool.Deps{Clock: now, Zone: zone}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	return registry
}

func executeClock[T any](t *testing.T, registry *tool.Registry, name string, args any) (T, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := registry.Execute(context.Background(), name, raw)
	if err != nil {
		var zero T
		return zero, err
	}
	out, ok := result.(T)
	if !ok {
		t.Fatalf("result type = %T, want %T", result, new(T))
	}
	return out, nil
}

// TestCityUsesLocalZoneTable checks a known city at a known instant.
func TestCityUsesLocalZoneTable(t *testing.T) {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	registry := clockRegistry(t, func() time.Time { return at }, nil)
	out, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "Tokyo"})
	if err != nil {
		t.Fatalf("time.in_city: %v", err)
	}
	if out.City != "Tokyo" || out.Timezone != "Asia/Tokyo" {
		t.Fatalf("out = %+v, want Tokyo/Asia/Tokyo", out)
	}
	if out.Time.Hour() != 0 || out.Time.Minute() != 4 {
		t.Fatalf("Tokyo time = %s, want 00:04", out.Time)
	}
	text, ok := registry.Speak(clock.CityName, out)
	if !ok {
		t.Fatal("Speak returned no text")
	}
	if text != "It's 12:04 AM in Tokyo." {
		t.Fatalf("Speak = %q", text)
	}
}

// TestCityUsesDSTFromTheZone checks summer and winter offsets for New York
// rather than a fixed offset.
func TestCityUsesDSTFromTheZone(t *testing.T) {
	summer := time.Date(2026, 7, 1, 16, 0, 0, 0, time.UTC)
	registry := clockRegistry(t, func() time.Time { return summer }, nil)
	out, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "New York"})
	if err != nil {
		t.Fatalf("time.in_city: %v", err)
	}
	if zone, offset := out.Time.Zone(); zone != "EDT" || offset != -4*3600 {
		t.Fatalf("summer zone = %s %d, want EDT -14400", zone, offset)
	}

	winter := time.Date(2026, 1, 15, 17, 0, 0, 0, time.UTC)
	registry = clockRegistry(t, func() time.Time { return winter }, nil)
	out, err = executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "New York"})
	if err != nil {
		t.Fatalf("time.in_city: %v", err)
	}
	if zone, offset := out.Time.Zone(); zone != "EST" || offset != -5*3600 {
		t.Fatalf("winter zone = %s %d, want EST -18000", zone, offset)
	}
}

// roundTripperFunc fails the test if any city lookup tries the network.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// TestCityMakesNoNetworkCall poisons the default HTTP transport around a real
// lookup. The city time must come entirely from the embedded/local tzdata.
func TestCityMakesNoNetworkCall(t *testing.T) {
	oldTransport := http.DefaultTransport
	oldClient := http.DefaultClient
	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("time.in_city attempted an HTTP request")
		return nil, errors.New("network disabled")
	})
	http.DefaultClient = &http.Client{Transport: http.DefaultTransport}
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		http.DefaultClient = oldClient
	})

	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	registry := clockRegistry(t, func() time.Time { return at }, nil)
	if _, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "Tokyo"}); err != nil {
		t.Fatalf("time.in_city: %v", err)
	}
}

// TestCityAmbiguousAndUnknown checks truthful failures instead of a guess.
func TestCityAmbiguousAndUnknown(t *testing.T) {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	registry := clockRegistry(t, func() time.Time { return at }, nil)

	if _, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "Portland"}); err == nil {
		t.Fatal("bare Portland returned no error")
	} else if !strings.Contains(tool.FailureMessage(err), "more than one") {
		t.Fatalf("ambiguous error = %q", tool.FailureMessage(err))
	}
	if out, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "Portland, Oregon"}); err != nil {
		t.Fatalf("Portland, Oregon: %v", err)
	} else if out.Timezone != "America/Los_Angeles" {
		t.Fatalf("Portland, Oregon zone = %q", out.Timezone)
	}
	if out, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "Portland, Maine"}); err != nil {
		t.Fatalf("Portland, Maine: %v", err)
	} else if out.Timezone != "America/New_York" {
		t.Fatalf("Portland, Maine zone = %q", out.Timezone)
	}
	if _, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: "Atlantis"}); err == nil {
		t.Fatal("unknown city returned no error")
	}
}

// TestCityAliases checks accents, country suffixes and apostrophes.
func TestCityAliases(t *testing.T) {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	registry := clockRegistry(t, func() time.Time { return at }, nil)
	for _, test := range []struct {
		city string
		zone string
	}{
		{"Tokyo, Japan", "Asia/Tokyo"},
		{"Tokyo Japan", "Asia/Tokyo"},
		{"São Paulo", "America/Sao_Paulo"},
		{"Sao Paulo", "America/Sao_Paulo"},
		{"St. John's", "America/St_Johns"},
		{"Xi'an", "Asia/Shanghai"},
		{"Zürich", "Europe/Zurich"},
	} {
		out, err := executeClock[clock.CityOut](t, registry, clock.CityName, clock.CityIn{City: test.city})
		if err != nil {
			t.Fatalf("%s: %v", test.city, err)
		}
		if out.Timezone != test.zone {
			t.Fatalf("%s zone = %q, want %q", test.city, out.Timezone, test.zone)
		}
	}
}

// TestExtractCity keeps unsupported questions on the Agent path.
func TestExtractCity(t *testing.T) {
	registry := clockRegistry(t, nil, nil)
	tests := []struct {
		utterance string
		city      string
	}{
		{"what time is it in Tokyo", "Tokyo"},
		{"what's the time in New York", "New York"},
		{"time in Tokyo", "Tokyo"},
		{"what time is it in Tokyo, Japan", "Tokyo, Japan"},
		{"what time is it in Tokyo Japan", "Tokyo Japan"},
		{"what's the local time in Berlin", "Berlin"},
	}
	for _, test := range tests {
		args, ok := registry.Extract(clock.CityName, test.utterance)
		if !ok {
			t.Fatalf("Extract(%q) failed", test.utterance)
		}
		var in clock.CityIn
		if err := json.Unmarshal(args, &in); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		if in.City != test.city {
			t.Fatalf("Extract(%q) = %q, want %q", test.utterance, in.City, test.city)
		}
	}
	for _, utterance := range []string{
		"what time zone is Tokyo in",
		"what's the time difference between here and Tokyo",
		"what time is it",
		"what time is it in Atlantis",
		"what time is it in the movie",
		"how many people live in Tokyo",
		"what's the weather in Tokyo",
	} {
		if _, ok := registry.Extract(clock.CityName, utterance); ok {
			t.Fatalf("Extract(%q) succeeded, want the Agent path", utterance)
		}
	}
}
