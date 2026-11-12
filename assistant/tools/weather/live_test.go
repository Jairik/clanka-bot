//go:build weatherlive

// Live check for #35. It is opt-in so the normal suite stays offline:
//
//	cd assistant && go test -tags weatherlive ./tools/weather -run TestLiveWeather -v
//
// It hits the real Open-Meteo endpoints. A failure here says something about
// the upstream or this box's network, not necessarily about the Tool.
package weather_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

func TestLiveWeather(t *testing.T) {
	reg := tool.NewRegistry(8 * time.Second)
	def := weather.New(weather.Deps{
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
		if elapsed > 4*time.Second {
			t.Fatalf("live failure took %v, want the 1.5s call / 2s budget bound", elapsed)
		}
		return
	}
	out, ok := result.(weather.Out)
	if !ok {
		t.Fatalf("result is %T, want weather.Out", result)
	}
	t.Logf("live weather ok in %v: %s %.1fC %s", elapsed, out.City, out.TemperatureC, out.Condition)
	if elapsed > 4*time.Second {
		t.Fatalf("live success took %v, want within the bounded budget", elapsed)
	}
}
