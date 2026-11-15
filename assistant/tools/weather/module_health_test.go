package weather

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// countingUpstream serves one Berlin geocode and one forecast, counting every
// request so a Health row's own traffic is visible. fail switches both to 500.
type countingUpstream struct {
	geo, forecast *httptest.Server
	calls         atomic.Int32
}

func newCountingUpstream(t *testing.T, fail bool) *countingUpstream {
	t.Helper()
	up := &countingUpstream{}
	up.geo = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up.calls.Add(1)
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52437,"longitude":13.41053,
			"timezone":"Europe/Berlin","country":"Germany","admin1":"State of Berlin"}]}`)
	}))
	t.Cleanup(up.geo.Close)
	up.forecast = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up.calls.Add(1)
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"latitude":52.512604,"longitude":13.419517,"timezone":"Europe/Berlin",
			"current":{"time":"2026-11-11T20:45","interval":900,"temperature_2m":3.9,
			"apparent_temperature":1.3,"relative_humidity_2m":85,"weather_code":3,"wind_speed_10m":5.2}}`)
	}))
	t.Cleanup(up.forecast.Close)
	return up
}

// module points an unconfigured-code-defaults Module at the fake upstream. It
// bypasses Configure so the test server can stay plain HTTP.
func (up *countingUpstream) module() *Module {
	m := NewModule()
	m.cfg = Config{
		GeocodingURL:  up.geo.URL,
		ForecastURL:   up.forecast.URL,
		FallbackURL:   up.forecast.URL,
		FallbackDelay: config.Duration(20 * time.Millisecond),
		Budget:        config.Duration(time.Second),
	}
	m.location = func() (location.Location, bool) {
		return location.Location{City: "Berlin"}, true
	}
	return m
}

// TestHealthReusesARecentAnswer is the #208 acceptance test for the weather
// row: a fresh memo answers without any provider request.
func TestHealthReusesARecentAnswer(t *testing.T) {
	up := newCountingUpstream(t, false)
	m := up.module()
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	if err := m.Health(context.Background()); err != nil {
		t.Fatalf("first Health: %v", err)
	}
	if got := up.calls.Load(); got == 0 {
		t.Fatal("first Health made no provider request, so the memo was never filled")
	}
	after := up.calls.Load()

	now = now.Add(time.Minute)
	if err := m.Health(context.Background()); err != nil {
		t.Fatalf("second Health: %v", err)
	}
	if got := up.calls.Load(); got != after {
		t.Fatalf("provider requests = %d, want %d: a fresh memo must not call out", got, after)
	}

	// Past the success TTL the next Health check asks again.
	now = now.Add(healthOKTTL)
	if err := m.Health(context.Background()); err != nil {
		t.Fatalf("expired Health: %v", err)
	}
	if got := up.calls.Load(); got == after {
		t.Fatal("an expired success memo was reused instead of rechecking")
	}
}

func TestHealthReusesAFailureOnlyForTheShortTTL(t *testing.T) {
	up := newCountingUpstream(t, true)
	m := up.module()
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	if err := m.Health(context.Background()); err == nil {
		t.Fatal("Health = nil with both providers failing")
	}
	after := up.calls.Load()

	now = now.Add(healthFailTTL / 2)
	if err := m.Health(context.Background()); err == nil {
		t.Fatal("Health = nil on the reused failure")
	}
	if got := up.calls.Load(); got != after {
		t.Fatalf("provider requests = %d, want %d inside the failure TTL", got, after)
	}

	now = now.Add(healthFailTTL)
	if err := m.Health(context.Background()); err == nil {
		t.Fatal("Health = nil with both providers failing")
	}
	if got := up.calls.Load(); got == after {
		t.Fatal("an expired failure memo was reused instead of rechecking")
	}
}

// TestASuccessfulCurrentCallSeedsTheHealthMemo keeps the row cheap right after
// a spoken or typed weather request.
func TestASuccessfulCurrentCallSeedsTheHealthMemo(t *testing.T) {
	up := newCountingUpstream(t, false)
	m := up.module()
	registry := tool.NewRegistry(time.Second)
	if err := m.Register(registry, tool.Deps{Location: m.location}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := registry.Execute(context.Background(), Name, []byte(`{}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	after := up.calls.Load()
	if after == 0 {
		t.Fatal("weather.current made no provider request")
	}

	if err := m.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := up.calls.Load(); got != after {
		t.Fatalf("provider requests = %d, want %d: a successful current call seeds Health", got, after)
	}
}
