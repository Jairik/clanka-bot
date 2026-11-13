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

// fallbackJSON mirrors wttr.in's documented string-valued current conditions.
const fallbackJSON = `{"nearest_area":[{"areaName":[{"value":"Berlin"}]}],"current_condition":[{"temp_C":"4","FeelsLikeC":"1","humidity":"85","windspeedKmph":"5.2","weatherDesc":[{"value":"Partly cloudy"}]}]}`

// TestWeatherFallbackRecoversAndCaches covers failures without repeating a slow primary.
func TestWeatherFallbackRecoversAndCaches(t *testing.T) {
	for _, failure := range []string{"503", "stalled", "malformed", "geocoding"} {
		t.Run(failure, func(t *testing.T) {
			geocoding, _ := fakeUpstream(t)
			var primaryCalls, fallbackCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				switch failure {
				case "503":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case "stalled", "geocoding":
					<-r.Context().Done()
				case "malformed":
					fmt.Fprint(w, `not json`)
				}
			}))
			defer primary.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls.Add(1)
				if r.URL.Path != "/Berlin" || r.URL.Query().Get("format") != "j1" || r.URL.Query().Get("lang") != "en" {
					t.Errorf("fallback URL = %s", r.URL)
				}
				fmt.Fprint(w, fallbackJSON)
			}))
			defer fallback.Close()
			// A geocoding outage must not prevent the independent provider from resolving Berlin.
			if failure == "geocoding" {
				geocoding = primary.URL
			}
			def := weather.New(weather.Deps{GeocodingURL: geocoding, ForecastURL: primary.URL, FallbackURL: fallback.URL})
			started := time.Now()
			first := execute(t, def, weather.In{City: "Berlin"})
			if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
				t.Fatalf("fallback took %v, want time left for the voice reply", elapsed)
			}
			want := weather.Out{City: "Berlin", Condition: "partly cloudy", TemperatureC: 4, ApparentC: 1, WindKph: 5.2, Humidity: 85}
			if first != want {
				t.Fatalf("fallback = %+v, want %+v", first, want)
			}
			if second := execute(t, def, weather.In{City: "Berlin"}); second != first {
				t.Fatalf("cached fallback = %+v, want %+v", second, first)
			}
			if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
				t.Fatalf("calls = primary:%d fallback:%d, want 1 each", primaryCalls.Load(), fallbackCalls.Load())
			}
		})
	}
}

// TestWeatherFallbackRejectsInvalidConditions prevents invented answers on bad upstream data.
func TestWeatherFallbackRejectsInvalidConditions(t *testing.T) {
	for _, response := range []string{
		`{}`, `{"current_condition":[]}`, `{"current_condition":[{}]}`,
		`{"current_condition":[{"temp_C":"bad"}]}`, `not json`,
		strings.Replace(fallbackJSON, `"temp_C":"4"`, `"temp_C":"bad"`, 1),
		strings.Replace(fallbackJSON, `"FeelsLikeC":"1",`, ``, 1),
		strings.Replace(fallbackJSON, `"humidity":"85"`, `"humidity":"101"`, 1),
		strings.Replace(fallbackJSON, `"windspeedKmph":"5.2"`, `"windspeedKmph":"-1"`, 1),
		strings.Replace(fallbackJSON, `"Partly cloudy"`, `" "`, 1),
	} {
		t.Run(response, func(t *testing.T) {
			geocoding, _ := fakeUpstream(t)
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}))
			defer primary.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, response)
			}))
			defer fallback.Close()
			def := weather.New(weather.Deps{GeocodingURL: geocoding, ForecastURL: primary.URL, FallbackURL: fallback.URL})
			_, err := def.Handler(context.Background(), weather.In{City: "Berlin"})
			if err == nil || tool.FailureMessage(err) != "Weather is unavailable right now." {
				t.Fatalf("invalid response returned %v", err)
			}
		})
	}
}

// TestWeatherSharedBudgetBoundsBothProviders checks production defaults and cancellation.
func TestWeatherSharedBudgetBoundsBothProviders(t *testing.T) {
	geocoding, _ := fakeUpstream(t)
	var fallbackCalls atomic.Int32
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fallbackCalls.Add(1)
		}
		<-r.Context().Done()
	}))
	defer stalled.Close()
	def := weather.New(weather.Deps{GeocodingURL: geocoding, ForecastURL: stalled.URL, FallbackURL: stalled.URL})
	reg := tool.NewRegistry(8 * time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := reg.Execute(context.Background(), weather.Name, []byte(`{"city":"Berlin"}`))
	if elapsed := time.Since(started); elapsed > 1250*time.Millisecond {
		t.Fatalf("both stalled providers took %v, want the shared 1s budget", elapsed)
	}
	if err == nil || tool.FailureMessage(err) != "Weather is unavailable right now." || fallbackCalls.Load() != 1 {
		t.Fatalf("error = %v, fallback calls = %d", err, fallbackCalls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reg.Execute(ctx, weather.Name, []byte(`{"city":"Berlin"}`))
	if err == nil || fallbackCalls.Load() != 1 {
		t.Fatalf("cancelled request returned %v, fallback calls = %d", err, fallbackCalls.Load())
	}
}

// TestWeatherKeepsThePrimaryAliveWhenFallbackFails preserves a healthy but slower primary.
func TestWeatherKeepsThePrimaryAliveWhenFallbackFails(t *testing.T) {
	geocoding, _ := fakeUpstream(t)
	fallbackStarted := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-fallbackStarted:
			fmt.Fprint(w, `{"current":{"temperature_2m":3.9,"apparent_temperature":1.3,"relative_humidity_2m":85,"weather_code":3,"wind_speed_10m":5.2}}`)
		case <-r.Context().Done():
		}
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fallback unavailable", http.StatusServiceUnavailable)
		close(fallbackStarted)
	}))
	defer fallback.Close()
	def := weather.New(weather.Deps{
		GeocodingURL: geocoding, ForecastURL: primary.URL, FallbackURL: fallback.URL,
		FallbackDelay: 10 * time.Millisecond,
	})
	out := execute(t, def, weather.In{City: "Berlin"})
	if out.TemperatureC != 3.9 || out.Condition != "overcast" {
		t.Fatalf("slower primary result = %+v", out)
	}
}
