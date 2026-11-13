//go:build weatherlive

// Live check for #35. It is opt-in so the normal suite stays offline:
//
//	cd assistant && go test -tags weatherlive ./tools/weather -run TestLiveWeather -v
//
// It hits the real configured weather endpoints. A failure here says something about
// the upstream or this box's network, not necessarily about the Tool.
package weather_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

func TestLiveWeather(t *testing.T) {
	reg := tool.NewRegistry(8 * time.Second)
	def := weather.New(weather.Deps{
		APIKey: fixedKey(os.Getenv(weather.SecretName)),
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
		},
	})
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}

	started := time.Now()
	result, err := reg.Execute(context.Background(), weather.Name, json.RawMessage(`{"city":"Berlin"}`))
	elapsed := time.Since(started)
	if err != nil {
		t.Logf("live weather failed after %v (bounded): %v", elapsed, err)
		if elapsed > 1250*time.Millisecond {
			t.Fatalf("live failure took %v, want the shared 1s budget bound", elapsed)
		}
		return
	}
	out, ok := result.(weather.Out)
	if !ok {
		t.Fatalf("result is %T, want weather.Out", result)
	}
	t.Logf("live weather ok in %v: %s %.1fC %s", elapsed, out.City, out.TemperatureC, out.Condition)
	if elapsed > 1250*time.Millisecond {
		t.Fatalf("live success took %v, want within the bounded budget", elapsed)
	}
}

// TestLiveWeatherAPI requires the configured primary to answer without a fallback.
func TestLiveWeatherAPI(t *testing.T) {
	key := os.Getenv(weather.SecretName)
	if key == "" {
		t.Skip("WEATHERAPI_API_KEY is not configured")
	}
	def := weather.New(weather.Deps{
		APIKey:       fixedKey(key),
		GeocodingURL: "http://127.0.0.1:1/unused", ForecastURL: "http://127.0.0.1:1/unused",
		FallbackURL: "http://127.0.0.1:1/unused",
	})
	started := time.Now()
	out := execute(t, def, weather.In{City: "Berlin"})
	t.Logf("WeatherAPI primary ok in %v: %s %.1fC %s", time.Since(started), out.City, out.TemperatureC, out.Condition)
}

// TestLiveWeatherFallback verifies the real second provider with Open-Meteo unavailable.
func TestLiveWeatherFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "simulated Open-Meteo outage", http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	def := weather.New(weather.Deps{GeocodingURL: primary.URL, ForecastURL: primary.URL})
	started := time.Now()
	out := execute(t, def, weather.In{City: "Berlin"})
	if elapsed := time.Since(started); elapsed > 1250*time.Millisecond {
		t.Fatalf("live fallback took %v, want the shared 1s budget bound", elapsed)
	}
	t.Logf("live fallback ok in %v: %s %.1fC %s", time.Since(started), out.City, out.TemperatureC, out.Condition)
}
