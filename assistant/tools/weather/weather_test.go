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
)

func berlin() location.Location {
	return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}
}

// fakeUpstream serves the documented Open-Meteo geocoding and forecast shapes.
func fakeUpstream(t *testing.T) (geocoding, forecast string) {
	t.Helper()
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("name"); got != "Berlin" {
			t.Errorf("geocoding name = %q, want Berlin", got)
		}
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52437,"longitude":13.41053,
			"timezone":"Europe/Berlin","country":"Germany","admin1":"State of Berlin"}]}`)
	}))
	t.Cleanup(geo.Close)

	fc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("latitude") == "" || query.Get("longitude") == "" {
			t.Errorf("forecast request has no coordinates: %s", r.URL.RawQuery)
		}
		if !strings.Contains(query.Get("current"), "weather_code") {
			t.Errorf("current fields = %q", query.Get("current"))
		}
		// The real response repeats the request coordinates at the top level.
		fmt.Fprint(w, `{"latitude":52.512604,"longitude":13.419517,"timezone":"Europe/Berlin",
			"current":{"time":"2026-11-11T20:45","interval":900,"temperature_2m":3.9,
			"apparent_temperature":1.3,"relative_humidity_2m":85,"weather_code":3,"wind_speed_10m":5.2}}`)
	}))
	t.Cleanup(fc.Close)
	return geo.URL, fc.URL
}

func newWeatherTool(t *testing.T, locate func() (location.Location, bool)) tool.Tool[weather.In, weather.Out] {
	t.Helper()
	geocoding, forecast := fakeUpstream(t)
	return weather.New(weather.Deps{
		GeocodingURL: geocoding,
		ForecastURL:  forecast,
		Location:     locate,
	})
}

func execute(t *testing.T, def tool.Tool[weather.In, weather.Out], in weather.In) weather.Out {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	args, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := reg.Execute(context.Background(), weather.Name, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := result.(weather.Out)
	if !ok {
		t.Fatalf("result is %T, want weather.Out", result)
	}
	return out
}

func TestWeatherAnswersForTheCurrentLocation(t *testing.T) {
	def := newWeatherTool(t, func() (location.Location, bool) { return berlin(), true })
	out := execute(t, def, weather.In{})
	if out.City != "Berlin" {
		t.Errorf("City = %q, want Berlin", out.City)
	}
	if out.Condition != "overcast" {
		t.Errorf("Condition = %q, want overcast", out.Condition)
	}
	if out.TemperatureC != 3.9 || out.ApparentC != 1.3 || out.Humidity != 85 {
		t.Errorf("Out = %+v", out)
	}

	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	spoken, ok := reg.Speak(weather.Name, out)
	if !ok {
		t.Fatal("Speak returned false")
	}
	if want := "It's 4 degrees and overcast in Berlin."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}

	// The Tool result goes to the model on the Agent path, so it must not carry
	// the upstream coordinates.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, banned := range []string{"latitude", "longitude", "52.512604", "13.419517"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("Tool result retained %q: %s", banned, raw)
		}
	}
}

func TestWeatherExtractsCityAndGeocodesIt(t *testing.T) {
	def := newWeatherTool(t, func() (location.Location, bool) { return location.Location{}, false })
	in, ok := def.Extract("what's the weather in Berlin?")
	if !ok {
		t.Fatal("Extract returned false")
	}
	if in.City != "Berlin" {
		t.Fatalf("Extract City = %q, want Berlin", in.City)
	}

	out := execute(t, def, in)
	if out.City != "Berlin" || out.Condition != "overcast" {
		t.Fatalf("Out = %+v", out)
	}

	// A phrasing without a city leaves the current location in charge.
	if in, ok := def.Extract("what's the weather like"); !ok || in.City != "" {
		t.Fatalf("Extract = %+v/%v, want no city", in, ok)
	}
}

func TestWeatherSaysWhenItCannotFindAPlace(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer geo.Close()

	def := weather.New(weather.Deps{
		GeocodingURL: geo.URL,
		ForecastURL:  "http://127.0.0.1:1/unused",
		Location:     func() (location.Location, bool) { return location.Location{}, false },
	})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	args, _ := json.Marshal(weather.In{City: "Atlantis"})
	_, err := reg.Execute(context.Background(), weather.Name, args)
	if err == nil {
		t.Fatal("Execute succeeded, want a user-facing failure")
	}
	if got := tool.FailureMessage(err); got != "I couldn't find that place." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

func TestWeatherSaysWhenLocationIsUnknown(t *testing.T) {
	def := weather.New(weather.Deps{
		GeocodingURL: "http://127.0.0.1:1/unused",
		ForecastURL:  "http://127.0.0.1:1/unused",
		Location:     func() (location.Location, bool) { return location.Location{}, false },
	})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), weather.Name, nil)
	if err == nil {
		t.Fatal("Execute succeeded, want a user-facing failure")
	}
	if got := tool.FailureMessage(err); got != "I don't know where I am yet." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

func TestWeatherIsANonlocalFastPathRead(t *testing.T) {
	def := newWeatherTool(t, func() (location.Location, bool) { return berlin(), true })
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info, ok := reg.Info(weather.Name)
	if !ok {
		t.Fatal("weather tool not registered")
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
	if info.LocalOnly {
		t.Error("LocalOnly = true, want false")
	}
	if !info.HasSpeak || !info.HasExtract {
		t.Errorf("HasSpeak/HasExtract = %v/%v", info.HasSpeak, info.HasExtract)
	}
	if len(info.Examples) == 0 {
		t.Error("Examples is empty")
	}
}

func TestWeatherBoundsAStalledUpstreamAndSpeaksTruthfully(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41}]}`)
	}))
	defer geo.Close()
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer forecast.Close()

	def := weather.New(weather.Deps{
		GeocodingURL: geo.URL,
		ForecastURL:  forecast.URL,
		Timeout:      100 * time.Millisecond,
		Budget:       250 * time.Millisecond,
	})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}

	started := time.Now()
	_, err := reg.Execute(context.Background(), weather.Name, json.RawMessage(`{"city":"Berlin"}`))
	if err == nil {
		t.Fatal("Execute succeeded, want a bounded failure")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("weather took %v, want a bounded failure", elapsed)
	}
	if got, want := tool.FailureMessage(err), "Weather is unavailable right now."; got != want {
		t.Fatalf("FailureMessage = %q, want %q", got, want)
	}
}

func TestWeatherReportsTransient503WithoutInventingAnAnswer(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41}]}`)
	}))
	defer geo.Close()
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer forecast.Close()

	def := weather.New(weather.Deps{GeocodingURL: geo.URL, ForecastURL: forecast.URL})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), weather.Name, json.RawMessage(`{"city":"Berlin"}`))
	if err == nil {
		t.Fatal("Execute succeeded on a 503, want a truthful failure")
	}
	if got, want := tool.FailureMessage(err), "Weather is unavailable right now."; got != want {
		t.Fatalf("FailureMessage = %q, want %q", got, want)
	}
}

func TestWeatherCachesGeocodeAndForecastBriefly(t *testing.T) {
	var geoCalls, forecastCalls int
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		geoCalls++
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41}]}`)
	}))
	defer geo.Close()
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forecastCalls++
		fmt.Fprint(w, `{"current":{"temperature_2m":3.9,"apparent_temperature":1.3,
			"relative_humidity_2m":85,"weather_code":3,"wind_speed_10m":5.2}}`)
	}))
	defer forecast.Close()

	def := weather.New(weather.Deps{GeocodingURL: geo.URL, ForecastURL: forecast.URL})
	first := execute(t, def, weather.In{City: "Berlin"})
	second := execute(t, def, weather.In{City: "Berlin"})
	if first != second {
		t.Fatalf("cached result = %+v, want %+v", second, first)
	}
	if geoCalls != 1 || forecastCalls != 1 {
		t.Fatalf("upstream calls = geocode:%d forecast:%d, want 1 each within the TTL", geoCalls, forecastCalls)
	}
}

// TestModuleDefaultsMatchTheShippedBounds pins the defaults a missing
// tools.weather section falls back to.
func TestModuleDefaultsMatchTheShippedBounds(t *testing.T) {
	cfg := weather.DefaultConfig()
	if cfg.GeocodingURL != weather.DefaultGeocodingURL || cfg.ForecastURL != weather.DefaultForecastURL {
		t.Fatalf("default upstreams = %q / %q", cfg.GeocodingURL, cfg.ForecastURL)
	}
	if cfg.Timeout.Duration() != 1500*time.Millisecond {
		t.Errorf("timeout = %s, want 1.5s", cfg.Timeout.Duration())
	}
	if cfg.Budget.Duration() != 2*time.Second {
		t.Errorf("budget = %s, want 2s", cfg.Budget.Duration())
	}
}
