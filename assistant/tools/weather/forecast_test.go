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
// a test can tell which daily index was chosen.
func dailyBerlin() string {
	return `{"latitude":52.52,"longitude":13.41,"timezone":"Europe/Berlin",
		"daily":{"time":["2026-11-11","2026-11-12","2026-11-13"],
		"temperature_2m_max":[10,20,30],
		"temperature_2m_min":[1,2,3],
		"precipitation_probability_max":[10,40,20],
		"weather_code":[0,61,1],
		"sunrise":["06:00","06:01","06:02"],
		"sunset":["17:00","17:01","17:02"]}}`
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
