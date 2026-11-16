package weather_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

// fakeGeocodeServer serves the documented Open-Meteo geocoding shape for name.
// When results is empty it reports no match so the unknown-city path is testable.
func fakeGeocodeServer(t *testing.T, name, tz string, results string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if results != "" {
			fmt.Fprint(w, results)
			return
		}
		fmt.Fprintf(w, `{"results":[{"name":%q,"latitude":52.52,"longitude":13.41,"timezone":%q}]}`, name, tz)
	}))
}

// fakeDailyServer serves a daily Open-Meteo payload for the forecast request.
func fakeDailyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("daily") == "" {
			t.Errorf("forecast request missing daily param: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("forecast_days") != "3" {
			t.Errorf("forecast request missing forecast_days=3: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, body)
	}))
}

func newForecastTool(t *testing.T, geocodeURL, dailyURL, fallbackURL string, now time.Time, locate func() (location.Location, bool)) tool.Tool[weather.ForecastIn, weather.ForecastOut] {
	t.Helper()
	return weather.NewForecast(weather.Deps{
		GeocodingURL: geocodeURL,
		ForecastURL:  dailyURL,
		FallbackURL:  fallbackURL,
		Now:          func() time.Time { return now },
		Location:     locate,
	})
}

func executeForecast(t *testing.T, def tool.Tool[weather.ForecastIn, weather.ForecastOut], in weather.ForecastIn) weather.ForecastOut {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	args, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := reg.Execute(context.Background(), weather.ForecastName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := result.(weather.ForecastOut)
	if !ok {
		t.Fatalf("result is %T, want weather.ForecastOut", result)
	}
	return out
}

// dailyBerlin builds a 3-day Open-Meteo payload with distinct per-day values so
// a test can tell which daily index was chosen. Sunrise/sunset are the ISO local
// datetimes Open-Meteo actually returns, which the primary path must normalise.
func dailyBerlin() string {
	return `{"latitude":52.52,"longitude":13.41,"timezone":"Europe/Berlin",
		"daily":{"time":["2026-11-11","2026-11-12","2026-11-13"],
		"temperature_2m_max":[10,20,30],
		"temperature_2m_min":[1,2,3],
		"precipitation_probability_max":[10,40,20],
		"weather_code":[0,61,1],
		"sunrise":["2026-11-11T06:42","2026-11-12T06:43","2026-11-13T06:44"],
		"sunset":["2026-11-11T17:30","2026-11-12T17:31","2026-11-13T17:32"]}}`
}

// TestForecastMapsDayToDailyIndexInLocationTimezone pins the day resolution in
// the location's timezone, not the host's.
func TestForecastMapsDayToDailyIndexInLocationTimezone(t *testing.T) {
	// 2026-11-11 is a Wednesday; the daily window is Wed/Thu/Fri.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, loc)

	geo := fakeGeocodeServer(t, "Berlin", "Europe/Berlin", "")
	defer geo.Close()
	daily := fakeDailyServer(t, dailyBerlin())
	defer daily.Close()

	def := newForecastTool(t, geo.URL, daily.URL, "", now, func() (location.Location, bool) {
		return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
	})

	cases := []struct {
		day      string
		wantHigh float64
		wantLow  float64
	}{
		{"today", 10, 1},
		{"tomorrow", 20, 2},
		{"thursday", 20, 2}, // tomorrow is Thursday, so the weekday maps to the same index
		{"friday", 30, 3},
	}
	for _, tc := range cases {
		t.Run(tc.day, func(t *testing.T) {
			out := executeForecast(t, def, weather.ForecastIn{Day: tc.day})
			if out.HighC != tc.wantHigh || out.LowC != tc.wantLow {
				t.Fatalf("day %q: high=%v low=%v, want high=%v low=%v", tc.day, out.HighC, out.LowC, tc.wantHigh, tc.wantLow)
			}
			if out.Day != tc.day {
				t.Errorf("Day = %q, want %q", out.Day, tc.day)
			}
		})
	}

	// A weekday outside the 3-day window is not invented.
	if _, err := def.Handler(context.Background(), weather.ForecastIn{Day: "saturday"}); err == nil {
		t.Fatal("saturday (out of window) returned no error, want a truthful message")
	} else if msg := tool.FailureMessage(err); !strings.Contains(msg, "too far out") {
		t.Fatalf("saturday error = %q, want a 'too far out' message", msg)
	}
}

// TestForecastUmbrellaThreshold answers yes at or above 40 percent and no below.
func TestForecastUmbrellaThreshold(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	geo := fakeGeocodeServer(t, "Berlin", "Europe/Berlin", "")
	defer geo.Close()
	daily := fakeDailyServer(t, dailyBerlin())
	defer daily.Close()

	def := newForecastTool(t, geo.URL, daily.URL, "", now, func() (location.Location, bool) {
		return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
	})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Tomorrow carries a 40 percent chance: the threshold is inclusive → yes.
	tomorrow := executeForecast(t, def, weather.ForecastIn{Day: "tomorrow", Umbrella: true})
	if tomorrow.RainChance != 40 {
		t.Fatalf("tomorrow rain chance = %d, want 40", tomorrow.RainChance)
	}
	spoken, ok := reg.Speak(weather.ForecastName, tomorrow)
	if !ok {
		t.Fatal("Speak returned false")
	}
	if !strings.HasPrefix(spoken, "Yes,") {
		t.Fatalf("umbrella at 40%% spoke %q, want a Yes answer", spoken)
	}

	// Today carries a 10 percent chance → no.
	today := executeForecast(t, def, weather.ForecastIn{Day: "today", Umbrella: true})
	spokenToday, ok := reg.Speak(weather.ForecastName, today)
	if !ok {
		t.Fatal("Speak returned false")
	}
	if !strings.HasPrefix(spokenToday, "No,") {
		t.Fatalf("umbrella at 10%% spoke %q, want a No answer", spokenToday)
	}
}

// TestForecastFallsBackToWttrOn5xx uses wttr.in's j1 daily blocks when Open-Meteo
// is unavailable.
func TestForecastFallsBackToWttrOn5xx(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	geo := fakeGeocodeServer(t, "Berlin", "Europe/Berlin", "")
	defer geo.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Berlin" || r.URL.Query().Get("format") != "j1" {
			t.Errorf("fallback URL = %s", r.URL)
		}
		fmt.Fprint(w, `{"weather":[{"date":"2026-11-12",
			"maxtempC":"14","mintempC":"6",
			"astronomy":[{"sunrise":"06:42 AM","sunset":"04:31 PM"}],
			"hourly":[
				{"time":"0","weatherDesc":[{"value":"Clear"}],"chanceofrain":"10"},
				{"time":"1200","weatherDesc":[{"value":"Light rain"}],"chanceofrain":"70"},
				{"time":"600","weatherDesc":[{"value":"Cloudy"}],"chanceofrain":"20"}]}]}`)
	}))
	defer fallback.Close()

	def := newForecastTool(t, geo.URL, primary.URL, fallback.URL, now, func() (location.Location, bool) {
		return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
	})
	out := executeForecast(t, def, weather.ForecastIn{Day: "tomorrow"})
	// The midday condition wins, and the max rain chance across the day is 70.
	if out.Condition != "light rain" {
		t.Errorf("Condition = %q, want light rain", out.Condition)
	}
	if out.RainChance != 70 {
		t.Errorf("RainChance = %d, want 70", out.RainChance)
	}
	if out.Sunrise != "06:42" || out.Sunset != "16:31" {
		t.Errorf("Sunrise/Sunset = %q/%q, want 06:42/16:31", out.Sunrise, out.Sunset)
	}
	if out.HighC != 14 || out.LowC != 6 {
		t.Errorf("High/Low = %v/%v, want 14/6", out.HighC, out.LowC)
	}
}

// TestForecastUnknownCity reports the geocoder's definitive miss.
func TestForecastUnknownCity(t *testing.T) {
	geo := fakeGeocodeServer(t, "", "", `{"results":[]}`)
	defer geo.Close()
	daily := fakeDailyServer(t, dailyBerlin())
	defer daily.Close()

	def := newForecastTool(t, geo.URL, daily.URL, "", time.Now(), func() (location.Location, bool) {
		return location.Location{}, false
	})
	_, err := def.Handler(context.Background(), weather.ForecastIn{City: "Atlantis", Day: "tomorrow"})
	if err == nil {
		t.Fatal("unknown city returned no error")
	}
	if msg := tool.FailureMessage(err); msg != "I couldn't find that place." {
		t.Fatalf("FailureMessage = %q, want the geocoder miss", msg)
	}
}

// TestForecastMalformedBody surfaces a truthful message when both providers are
// unreachable or unparseable, instead of inventing a forecast.
func TestForecastMalformedBody(t *testing.T) {
	geo := fakeGeocodeServer(t, "Berlin", "Europe/Berlin", "")
	defer geo.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer fallback.Close()

	def := newForecastTool(t, geo.URL, primary.URL, fallback.URL, time.Now(), func() (location.Location, bool) {
		return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
	})
	_, err := def.Handler(context.Background(), weather.ForecastIn{Day: "tomorrow"})
	if err == nil {
		t.Fatal("malformed upstreams returned no error")
	}
	if msg := tool.FailureMessage(err); msg != "Weather is unavailable right now." {
		t.Fatalf("FailureMessage = %q, want the unavailable message", msg)
	}
}

// TestForecastExtractRejectsRanges keeps multi-day requests on the Agent path.
func TestForecastExtractRejectsRanges(t *testing.T) {
	def := weather.NewForecast(weather.Deps{})
	for _, text := range []string{
		"what's the forecast for the weekend",
		"what's the weather going to be like next week",
		"will it rain this week",
		"what's the forecast for the next 5 days",
	} {
		if in, ok := def.Extract(text); ok {
			t.Errorf("Extract(%q) = %+v, true; ranges must be rejected", text, in)
		}
	}
}

// TestForecastExtractParsesDayCityAndUmbrella confirms the Fast path argument
// filler captures the day, optional city and umbrella intent.
func TestForecastExtractParsesDayCityAndUmbrella(t *testing.T) {
	def := weather.NewForecast(weather.Deps{})
	in, ok := def.Extract("do I need an umbrella in Berlin tomorrow")
	if !ok {
		t.Fatal("Extract returned false")
	}
	if in.Day != "tomorrow" || in.City != "Berlin" || !in.Umbrella {
		t.Fatalf("Extract = %+v, want day=tomorrow city=Berlin umbrella=true", in)
	}

	in, ok = def.Extract("what's the high on Friday")
	if !ok || in.Day != "friday" || in.City != "" || in.Umbrella {
		t.Fatalf("Extract = %+v, ok=%v; want day=friday, no city, no umbrella", in, ok)
	}

	in, ok = def.Extract("when is the sunset")
	if !ok || in.Day != "today" || in.Umbrella {
		t.Fatalf("Extract = %+v, ok=%v; want day=today, no umbrella", in, ok)
	}
}

// newForecastToolWithSettings is like newForecastTool but wires a live settings
// service so the unit-resolution tests can drive the temperature preference.
func newForecastToolWithSettings(t *testing.T, geocodeURL, dailyURL, fallbackURL string, now time.Time, locate func() (location.Location, bool), settingsFn func() *settings.Service) tool.Tool[weather.ForecastIn, weather.ForecastOut] {
	t.Helper()
	return weather.NewForecast(weather.Deps{
		GeocodingURL: geocodeURL,
		ForecastURL:  dailyURL,
		FallbackURL:  fallbackURL,
		Now:          func() time.Time { return now },
		Location:     locate,
		Settings:     settingsFn,
	})
}

// fakeSettings opens a real settings service backed by a temp store and, when
// unit is non-empty, sets the live temperature-unit preference to it.
func fakeSettings(t *testing.T, unit string) *settings.Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := settings.New(context.Background(), st, weather.Definitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if unit != "" {
		if _, err := svc.Set(context.Background(), weather.UnitKey, unit); err != nil {
			t.Fatalf("set %s: %v", unit, err)
		}
	}
	return svc
}

// dailyForecastValues builds a 3-day Open-Meteo payload with one high/low pair
// repeated across the window so a test can assert the displayed unit.
func dailyForecastValues(high, low float64) string {
	return fmt.Sprintf(`{"latitude":52.52,"longitude":13.41,"timezone":"Europe/Berlin",
		"daily":{"time":["2026-11-11","2026-11-12","2026-11-13"],
		"temperature_2m_max":[%v,%v,%v],
		"temperature_2m_min":[%v,%v,%v],
		"precipitation_probability_max":[20,40,20],
		"weather_code":[2,61,1],
		"sunrise":["2026-11-11T06:42","2026-11-12T06:43","2026-11-13T06:44"],
		"sunset":["2026-11-11T17:30","2026-11-12T17:31","2026-11-13T17:32"]}}`,
		high, high, high, low, low, low)
}

// TestForecastSpeakHonorsUnitSetting confirms the renderer reads the live unit
// per reply: an explicit celsius/fahrenheit wins, and "auto" follows the
// forecast's own place timezone (US zones -> Fahrenheit, elsewhere -> Celsius).
func TestForecastSpeakHonorsUnitSetting(t *testing.T) {
	cases := []struct {
		name     string
		setting  string // "" leaves the code default ("auto")
		geoName  string
		geoTZ    string
		wantWord string
		wantHigh string
		wantLow  string
	}{
		{"celsius setting", "celsius", "Berlin", "Europe/Berlin", "Celsius", "14", "6"},
		{"fahrenheit setting", "fahrenheit", "Berlin", "Europe/Berlin", "Fahrenheit", "57", "43"},
		{"auto US zone", "auto", "New York", "America/New_York", "Fahrenheit", "57", "43"},
		{"auto Berlin zone", "auto", "Berlin", "Europe/Berlin", "Celsius", "14", "6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			geo := fakeGeocodeServer(t, tc.geoName, tc.geoTZ, "")
			defer geo.Close()
			daily := fakeDailyServer(t, dailyForecastValues(14, 6))
			defer daily.Close()
			svc := fakeSettings(t, tc.setting)
			locate := func() (location.Location, bool) {
				return location.Location{City: tc.geoName, Timezone: tc.geoTZ}, true
			}
			def := newForecastToolWithSettings(t, geo.URL, daily.URL, "", time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC), locate, func() *settings.Service { return svc })
			out := executeForecast(t, def, weather.ForecastIn{Day: "today"})
			spoken := def.Speak(out)
			if !strings.Contains(spoken, "degrees "+tc.wantWord) {
				t.Fatalf("spoken %q, want it to name unit %q", spoken, tc.wantWord)
			}
			if !strings.Contains(spoken, "high of "+tc.wantHigh) {
				t.Errorf("spoken %q, want high of %s", spoken, tc.wantHigh)
			}
			if !strings.Contains(spoken, "low of "+tc.wantLow) {
				t.Errorf("spoken %q, want low of %s", spoken, tc.wantLow)
			}
		})
	}
}

// TestForecastNormalizesSunriseSunset pins that the Open-Meteo ISO local datetime
// sunrise/sunset are reduced to a 15:04 clock in both the Out and the spoken text.
func TestForecastNormalizesSunriseSunset(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	geo := fakeGeocodeServer(t, "Berlin", "Europe/Berlin", "")
	defer geo.Close()
	daily := fakeDailyServer(t, dailyBerlin())
	defer daily.Close()

	def := newForecastTool(t, geo.URL, daily.URL, "", now, func() (location.Location, bool) {
		return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
	})
	out := executeForecast(t, def, weather.ForecastIn{Day: "today"})
	if out.Sunrise != "06:42" || out.Sunset != "17:30" {
		t.Fatalf("Sunrise/Sunset = %q/%q, want 06:42/17:30", out.Sunrise, out.Sunset)
	}
	spoken := def.Speak(out)
	if !strings.Contains(spoken, "Sunrise 06:42") || !strings.Contains(spoken, "sunset 17:30") {
		t.Fatalf("spoken %q, want Sunrise 06:42 and sunset 17:30", spoken)
	}
}

// TestForecastExtractParsesForCity confirms the Fast path filler reads a city
// after " for " as well as " in ", drops a trailing day token ("for London
// tomorrow" -> London), and leaves the city empty for a bare day.
func TestForecastExtractParsesForCity(t *testing.T) {
	def := weather.NewForecast(weather.Deps{})
	cases := []struct {
		text        string
		wantDay     string
		wantCity    string
		wantUmbrella bool
	}{
		{"what's the forecast for London tomorrow", "tomorrow", "London", false},
		{"what's the forecast for Paris", "today", "Paris", false},
		{"what's the forecast for tomorrow", "tomorrow", "", false},
		{"what's the high tomorrow", "tomorrow", "", false},
		{"do I need an umbrella today", "today", "", true},
		{"when does the sun set in Paris", "today", "Paris", false},
		{"what's the forecast for Friday", "friday", "", false},
	}
	for _, tc := range cases {
		in, ok := def.Extract(tc.text)
		if !ok {
			t.Fatalf("Extract(%q) = false; want true", tc.text)
		}
		if in.Day != tc.wantDay || in.City != tc.wantCity || in.Umbrella != tc.wantUmbrella {
			t.Errorf("Extract(%q) = %+v; want day=%q city=%q umbrella=%v", tc.text, in, tc.wantDay, tc.wantCity, tc.wantUmbrella)
		}
	}

	// Multi-day ranges must still be rejected before any city parsing.
	for _, text := range []string{
		"what's the forecast for the weekend",
		"what's the forecast for next 5 days",
		"what should I wear this week",
	} {
		if in, ok := def.Extract(text); ok {
			t.Errorf("Extract(%q) = %+v, true; multi-day ranges must be rejected", text, in)
		}
	}
}
