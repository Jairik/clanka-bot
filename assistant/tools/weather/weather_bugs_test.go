package weather

import (
	"fmt"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
)

// TestTimezoneToCity turns an IANA timezone fallback into a geocodable city name
// and skips the zones that have no city meaning.
func TestTimezoneToCity(t *testing.T) {
	tests := []struct {
		tz    string
		want  string
		ok    bool
	}{
		{"America/New_York", "New York", true},
		{"Europe/Berlin", "Berlin", true},
		{"Asia/Kolkata", "Kolkata", true},
		{"Pacific/Auckland", "Auckland", true},
		{"UTC", "", false},
		{"GMT", "", false},
		{"Etc/UTC", "", false},
		{"Etc/GMT+2", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := timezoneToCity(tt.tz)
		if ok != tt.ok || got != tt.want {
			t.Errorf("timezoneToCity(%q) = %q, %v; want %q, %v", tt.tz, got, ok, tt.want, tt.ok)
		}
	}
}

// TestResolveCityTimezoneFallsBackToCityName proves the timezone candidate is
// converted to a city name rather than geocoded as the raw IANA identifier.
func TestResolveCityTimezoneFallsBackToCityName(t *testing.T) {
	deps := Deps{
		Location: func() (location.Location, bool) {
			return location.Location{Timezone: "America/New_York"}, true
		},
	}
	city, err := resolveCity(In{}, deps)
	if err != nil {
		t.Fatalf("resolveCity: %v", err)
	}
	if city != "New York" {
		t.Fatalf("city = %q, want New York", city)
	}
}

// TestResolveCityUngeocodableTimezoneFallsThrough proves a UTC-only location
// (no city, no region) cannot be geocoded and reports the unknown place.
func TestResolveCityUngeocodableTimezoneFallsThrough(t *testing.T) {
	deps := Deps{
		Location: func() (location.Location, bool) {
			return location.Location{Timezone: "UTC"}, true
		},
	}
	if _, err := resolveCity(In{}, deps); err == nil {
		t.Fatal("resolveCity with only UTC returned no error, want unknown location")
	}
}

// TestCacheEvictionCapsForecastEntries proves the answer cache never grows past
// its cap even for many distinct cities.
func TestCacheEvictionCapsForecastEntries(t *testing.T) {
	c := newCache()
	now := time.Now()
	for i := 0; i < 100; i++ {
		c.setForecast(fmt.Sprintf("city-%d", i), Out{City: fmt.Sprintf("city-%d", i)}, now)
	}
	if got := len(c.forecasts); got > cacheCap {
		t.Fatalf("forecast entries = %d, want at most %d", got, cacheCap)
	}
	for i := 0; i < 100; i++ {
		c.setPlace(fmt.Sprintf("p-%d", i), place{Name: fmt.Sprintf("p-%d", i)}, now)
	}
	if got := len(c.places); got > cacheCap {
		t.Fatalf("place entries = %d, want at most %d", got, cacheCap)
	}
}

// TestCacheEvictsExpiredEntries proves an expired entry is dropped on insert
// while a fresh one survives.
func TestCacheEvictsExpiredEntries(t *testing.T) {
	c := newCache()
	now := time.Now()
	c.places["old"] = cacheEntry[place]{at: now.Add(-geocodeTTL - time.Minute)}
	c.places["keep"] = cacheEntry[place]{at: now}

	c.setPlace("new", place{}, now)

	if _, ok := c.places["old"]; ok {
		t.Error("expired place entry was not evicted on insert")
	}
	if _, ok := c.places["keep"]; !ok {
		t.Error("fresh place entry was evicted")
	}
	if _, ok := c.places["new"]; !ok {
		t.Error("new place entry is missing")
	}
}
