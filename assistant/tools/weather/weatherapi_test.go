package weather_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

// weatherAPIJSON matches the current-conditions fields used from WeatherAPI.
const weatherAPIJSON = `{"location":{"name":"Berlin","lat":52.52,"lon":13.41},"current":{"temp_c":4.5,"feelslike_c":1.2,"wind_kph":5.2,"humidity":85,"condition":{"text":"Partly cloudy"}}}`

// TestWeatherAPIPreferredAndCached avoids geocoding and fallback calls on a healthy primary.
func TestWeatherAPIPreferredAndCached(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("key") != "test-key" || r.URL.Query().Get("q") != "Berlin" || r.URL.Query().Get("lang") != "en" {
			t.Error("WeatherAPI request missing expected city, key or language")
		}
		fmt.Fprint(w, weatherAPIJSON)
	}))
	defer api.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("healthy WeatherAPI called a fallback")
		http.Error(w, "unexpected fallback", http.StatusInternalServerError)
	}))
	defer fallback.Close()
	def := weather.New(weather.Deps{
		APIKey: fixedKey("test-key"), WeatherAPIURL: api.URL,
		GeocodingURL: fallback.URL, ForecastURL: fallback.URL, FallbackURL: fallback.URL,
	})
	first := execute(t, def, weather.In{City: "Berlin"})
	want := weather.Out{City: "Berlin", TemperatureC: 4.5, ApparentC: 1.2, WindKph: 5.2, Humidity: 85, Condition: "partly cloudy"}
	if first != want {
		t.Fatalf("WeatherAPI result = %+v, want %+v", first, want)
	}
	if second := execute(t, def, weather.In{City: " BERLIN "}); second != first || calls.Load() != 1 {
		t.Fatalf("cached result = %+v, primary calls = %d", second, calls.Load())
	}
}

// TestWeatherAPIRecoversThroughOpenMeteo covers auth, quota, outage and malformed primary responses.
func TestWeatherAPIRecoversThroughOpenMeteo(t *testing.T) {
	for _, failure := range []string{"401", "403", "503", "stalled", "malformed", "incomplete", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			geocoding, forecast := fakeUpstream(t)
			var redirected atomic.Int32
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				fmt.Fprint(w, weatherAPIJSON)
			}))
			defer redirect.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch failure {
				case "401":
					http.Error(w, "invalid API key", http.StatusUnauthorized)
				case "403":
					http.Error(w, "quota exceeded", http.StatusForbidden)
				case "503":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case "stalled":
					<-r.Context().Done()
				case "malformed":
					fmt.Fprint(w, "not JSON")
				case "incomplete":
					fmt.Fprint(w, `{"location":{"name":"Berlin"},"current":{}}`)
				case "redirect":
					http.Redirect(w, r, redirect.URL+"?key=test-key", http.StatusFound)
				}
			}))
			defer api.Close()
			def := weather.New(weather.Deps{
				APIKey: fixedKey("test-key"), WeatherAPIURL: api.URL,
				GeocodingURL: geocoding, ForecastURL: forecast, FallbackURL: redirect.URL,
				// The canary must never be consulted: Open-Meteo answers these
				// cases. Ten milliseconds let a starved scheduler start the
				// fallback first and fail the result assertion, so give the
				// direct path a head start the local server cannot lose while
				// staying inside the registry's one-second budget.
				FallbackDelay: 500 * time.Millisecond,
			})
			out := execute(t, def, weather.In{City: "Berlin"})
			if out.Condition != "overcast" || out.TemperatureC != 3.9 || redirected.Load() != 0 {
				t.Fatalf("fallback result = %+v, redirected requests = %d", out, redirected.Load())
			}
		})
	}
}

// TestWeatherAPIAndOpenMeteoFailureReachesWttr retains the final independent fallback.
func TestWeatherAPIAndOpenMeteoFailureReachesWttr(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer failed.Close()
	wttr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "" {
			t.Error("WeatherAPI credential reached fallback")
		}
		fmt.Fprint(w, fallbackJSON)
	}))
	defer wttr.Close()
	def := weather.New(weather.Deps{
		APIKey: fixedKey("test-key"), WeatherAPIURL: failed.URL,
		GeocodingURL: failed.URL, ForecastURL: failed.URL, FallbackURL: wttr.URL,
	})
	if out := execute(t, def, weather.In{City: "Berlin"}); out.Condition != "partly cloudy" || out.TemperatureC != 4 {
		t.Fatalf("wttr result = %+v", out)
	}
}

// TestWeatherAPISharedBudgetAndSecretSafety bounds all three providers without exposing the key.
func TestWeatherAPISharedBudgetAndSecretSafety(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer stalled.Close()
	def := weather.New(weather.Deps{
		APIKey: fixedKey("private-test-key"), WeatherAPIURL: stalled.URL,
		GeocodingURL: stalled.URL, ForecastURL: stalled.URL, FallbackURL: stalled.URL,
	})
	reg := tool.NewRegistry(8 * time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := reg.Execute(context.Background(), weather.Name, []byte(`{"city":"Berlin"}`))
	if elapsed := time.Since(started); elapsed > 1250*time.Millisecond {
		t.Fatalf("three stalled providers took %v, want the shared 1s budget", elapsed)
	}
	if err == nil || tool.FailureMessage(err) != "Weather is unavailable right now." {
		t.Fatalf("unexpected failure = %v", err)
	}
	if strings.Contains(err.Error(), "private-test-key") || strings.Contains(err.Error(), "key=") {
		t.Fatal("error exposed a credential-bearing URL")
	}
}
