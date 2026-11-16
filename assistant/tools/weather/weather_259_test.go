package weather_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
	"gopkg.in/yaml.v3"
)

// TestTimezoneLocationGeocodesCityName proves a plain "what's the weather" with a
// location that only carries the timezone America/New_York resolves to New York
// instead of geocoding the raw IANA identifier.
func TestTimezoneLocationGeocodesCityName(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("name"); got != "New York" {
			t.Errorf("geocoding name = %q, want New York", got)
		}
		fmt.Fprint(w, `{"results":[{"name":"New York","latitude":40.71,"longitude":-74.0,"timezone":"America/New_York"}]}`)
	}))
	defer geo.Close()
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"current":{"temperature_2m":10.0,"apparent_temperature":8.0,"relative_humidity_2m":50,"weather_code":1,"wind_speed_10m":3.0}}`)
	}))
	defer forecast.Close()

	def := weather.New(weather.Deps{
		GeocodingURL: geo.URL,
		ForecastURL:  forecast.URL,
		Location: func() (location.Location, bool) {
			return location.Location{Timezone: "America/New_York"}, true
		},
	})
	out := execute(t, def, weather.In{})
	if out.City != "New York" {
		t.Fatalf("City = %q, want New York", out.City)
	}
}

// TestRejectedWeatherAPIKeyShowsInAuxHealth proves a 401 from WeatherAPI still
// yields a successful fallback answer, records the outcome, and surfaces a
// rejected AuxHealth row without ever printing the key.
func TestRejectedWeatherAPIKeyShowsInAuxHealth(t *testing.T) {
	const secret = "super-secret-weather-key"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid key", http.StatusUnauthorized)
	}))
	defer api.Close()
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41}]}`)
	}))
	defer geo.Close()
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"current":{"temperature_2m":3.9,"apparent_temperature":1.3,"relative_humidity_2m":85,"weather_code":3,"wind_speed_10m":5.2}}`)
	}))
	defer forecast.Close()

	module := weather.NewModule()
	var node yaml.Node
	if err := node.Encode(map[string]any{
		"weatherapi_url": api.URL,
		"geocoding_url":  geo.URL,
		"forecast_url":   forecast.URL,
		"fallback_url":   forecast.URL,
		"fallback_delay": "20ms",
		"budget":         "1s",
	}); err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := module.Configure(&node); err != nil {
		t.Fatalf("configure: %v", err)
	}
	reg := tool.NewRegistry(time.Second)
	if err := module.Register(reg, tool.Deps{
		Location: func() (location.Location, bool) { return location.Location{City: "Berlin"}, true },
		Secrets:  fakeSecrets{key: secret},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The chain falls back to the keyless providers and answers successfully.
	if _, err := reg.Execute(context.Background(), weather.Name, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	rows := module.AuxHealth(context.Background())
	if len(rows) != 1 {
		t.Fatalf("AuxHealth rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Name != "WeatherAPI key" {
		t.Errorf("row name = %q, want \"WeatherAPI key\"", row.Name)
	}
	if row.State != tool.StatusError {
		t.Errorf("row state = %q, want error", row.State)
	}
	if !strings.Contains(row.Detail, "401") {
		t.Errorf("row detail = %q, want it to name the 401 status", row.Detail)
	}
	if strings.Contains(row.Detail, secret) {
		t.Errorf("row detail leaks the key: %q", row.Detail)
	}
}

// TestMissingWeatherAPIKeyIsNotConfigured proves the row is not-configured (never
// an error) when no key is present, so the dashboard shows the honest state.
func TestMissingWeatherAPIKeyIsNotConfigured(t *testing.T) {
	module := weather.NewModule()
	var node yaml.Node
	if err := node.Encode(map[string]any{
		"weatherapi_url": "https://api.weatherapi.com/v1/current.json",
		"geocoding_url":  "http://geocoding.example/v1/search",
		"forecast_url":   "http://forecast.example/v1/forecast",
		"fallback_url":   "http://wttr.example",
	}); err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := module.Configure(&node); err != nil {
		t.Fatalf("configure: %v", err)
	}
	reg := tool.NewRegistry(time.Second)
	if err := module.Register(reg, tool.Deps{
		Location: func() (location.Location, bool) { return location.Location{City: "Berlin"}, true },
		Secrets:  fakeSecrets{key: ""},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rows := module.AuxHealth(context.Background())
	if len(rows) != 1 {
		t.Fatalf("AuxHealth rows = %d, want 1", len(rows))
	}
	if rows[0].State != tool.StatusNotConfigured {
		t.Errorf("row state = %q, want not-configured", rows[0].State)
	}
}

// TestRejectedWeatherAPIKeyErrorHidesKey proves the surfaced failure for a dead
// key never prints the credential, even when every provider fails.
func TestRejectedWeatherAPIKeyErrorHidesKey(t *testing.T) {
	const secret = "super-secret-weather-key"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid key", http.StatusUnauthorized)
	}))
	defer api.Close()
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer failed.Close()

	def := weather.New(weather.Deps{
		APIKey:        fixedKey(secret),
		WeatherAPIURL: api.URL,
		GeocodingURL:  failed.URL,
		ForecastURL:   failed.URL,
		FallbackURL:   failed.URL,
	})
	reg := tool.NewRegistry(2 * time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), weather.Name, json.RawMessage(`{"city":"Berlin"}`))
	if err == nil {
		t.Fatal("Execute succeeded with every provider down, want a failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the key: %v", err)
	}
	if strings.Contains(err.Error(), "key=") {
		t.Errorf("error leaks a credential-bearing query string: %v", err)
	}
}
