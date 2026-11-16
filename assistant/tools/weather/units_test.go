package weather_test

import (
	"context"
	"encoding/json"
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

// newUnitSettings builds the real settings service the weather Tool reads in
// production. A blank unit leaves the declared default in place.
func newUnitSettings(t *testing.T, unit string) *settings.Service {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	svc, err := settings.New(ctx, st, weather.Definitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if unit != "" {
		setUnit(t, svc, unit)
	}
	return svc
}

func setUnit(t *testing.T, svc *settings.Service, unit string) {
	t.Helper()
	raw, err := json.Marshal(unit)
	if err != nil {
		t.Fatalf("marshal unit: %v", err)
	}
	if _, err := svc.SetJSON(context.Background(), weather.UnitKey, raw); err != nil {
		t.Fatalf("SetJSON(%q): %v", unit, err)
	}
}

// spoken registers a weather Tool wired to svc and loc and renders out through
// the real Fast path Speak hook. No network is involved: Speak only reads deps.
func spoken(t *testing.T, svc *settings.Service, loc location.Location, out weather.Out) string {
	t.Helper()
	def := weather.New(weather.Deps{
		Location: func() (location.Location, bool) { return loc, true },
		Settings: func() *settings.Service { return svc },
	})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	text, ok := reg.Speak(weather.Name, out)
	if !ok {
		t.Fatal("Speak returned false")
	}
	return text
}

// TestTemperatureUnitDefinition pins the key, the "auto" default, the live
// kind and the enum validator: the same checks the Dashboard/Settings API rely
// on to show and refuse values (issue #318).
func TestTemperatureUnitDefinition(t *testing.T) {
	defs := weather.Definitions()
	if len(defs) != 1 {
		t.Fatalf("Definitions() = %d entries, want 1", len(defs))
	}
	def := defs[0]
	if def.Key != weather.UnitKey || def.Default != weather.UnitAuto || def.Kind != settings.Live {
		t.Fatalf("definition = %+v, want key %q, default %q, kind live", def, weather.UnitKey, weather.UnitAuto)
	}

	svc := newUnitSettings(t, "")
	value, ok := svc.Get(weather.UnitKey)
	if !ok || value.Value != weather.UnitAuto {
		t.Fatalf("default = %+v/%v, want %q", value, ok, weather.UnitAuto)
	}
	for _, unit := range []string{weather.UnitAuto, weather.UnitCelsius, weather.UnitFahrenheit} {
		setUnit(t, svc, unit)
	}
	for _, invalid := range []string{"kelvin", "", "FAHRENHEIT", "f"} {
		raw, err := json.Marshal(invalid)
		if err != nil {
			t.Fatalf("marshal %q: %v", invalid, err)
		}
		if _, err := svc.SetJSON(context.Background(), weather.UnitKey, raw); err == nil {
			t.Errorf("SetJSON(%q) accepted an invalid unit", invalid)
		}
	}
}

// TestSpeakTemperatureUnit covers the default (auto with a non-US and a US
// timezone), both explicit overrides and the 21.4°C -> 71°F rounding from the
// issue. The unit word is always spoken exactly once.
func TestSpeakTemperatureUnit(t *testing.T) {
	us := location.Location{City: "Baltimore", Region: "Maryland", Timezone: "America/New_York"}
	eu := berlin() // Europe/Berlin
	tests := []struct {
		name, unit string
		loc        location.Location
		out        weather.Out
		want       string
	}{
		{
			name: "default keeps the Celsius value as today",
			loc:  eu,
			out:  weather.Out{City: "Berlin", Condition: "overcast", TemperatureC: 3.9},
			want: "It's 4 degrees Celsius and overcast in Berlin.",
		},
		{
			name: "auto uses Fahrenheit in a US timezone and rounds",
			loc:  us,
			out:  weather.Out{City: "Baltimore", Condition: "clear", TemperatureC: 21.4},
			want: "It's 71 degrees Fahrenheit and clear in Baltimore.",
		},
		{
			name: "explicit celsius keeps the metric value in a US zone",
			unit: weather.UnitCelsius,
			loc:  us,
			out:  weather.Out{City: "Baltimore", Condition: "clear", TemperatureC: 21.4},
			want: "It's 21 degrees Celsius and clear in Baltimore.",
		},
		{
			name: "explicit fahrenheit converts outside the US",
			unit: weather.UnitFahrenheit,
			loc:  eu,
			out:  weather.Out{City: "Berlin", Condition: "overcast", TemperatureC: 3.9},
			want: "It's 39 degrees Fahrenheit and overcast in Berlin.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newUnitSettings(t, tt.unit)
			if got := spoken(t, svc, tt.loc, tt.out); got != tt.want {
				t.Fatalf("Speak = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTemperatureUnitOverrideIsLive proves the setting is read per reply: one
// Tool instance switches unit after a Dashboard-style write with no rebuild.
func TestTemperatureUnitOverrideIsLive(t *testing.T) {
	svc := newUnitSettings(t, "")
	def := weather.New(weather.Deps{
		Location: func() (location.Location, bool) { return berlin(), true },
		Settings: func() *settings.Service { return svc },
	})
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := weather.Out{City: "Berlin", Condition: "clear", TemperatureC: 21.4}
	before, _ := reg.Speak(weather.Name, out)
	if !strings.Contains(before, "21 degrees Celsius") {
		t.Fatalf("Speak before override = %q", before)
	}
	setUnit(t, svc, weather.UnitFahrenheit)
	after, _ := reg.Speak(weather.Name, out)
	if !strings.Contains(after, "71 degrees Fahrenheit") {
		t.Fatalf("Speak after override = %q", after)
	}
}
