package pipeline_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

func weatherUpstreams(t *testing.T) (string, string) {
	t.Helper()
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52437,"longitude":13.41053,
			"timezone":"Europe/Berlin"}]}`)
	}))
	t.Cleanup(geo.Close)
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"latitude":52.512604,"longitude":13.419517,
			"current":{"temperature_2m":3.9,"apparent_temperature":1.3,
			"relative_humidity_2m":85,"weather_code":3,"wind_speed_10m":5.2}}`)
	}))
	t.Cleanup(forecast.Close)
	return geo.URL, forecast.URL
}

// TestWeatherAnswersOnTheFastPath is acceptance criterion 1: a weather question
// reaches weather.current through the semantic Fast path, not the agent.
func TestWeatherAnswersOnTheFastPath(t *testing.T) {
	geocoding, forecast := weatherUpstreams(t)
	reg := tool.NewRegistry(time.Second)
	def := weather.New(weather.Deps{
		GeocodingURL: geocoding,
		ForecastURL:  forecast,
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
		},
	})
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}

	info, ok := reg.Info(weather.Name)
	if !ok {
		t.Fatalf("%s is not registered", weather.Name)
	}
	emb := &fakeEmbedder{vectors: map[string][]float32{}}
	for _, example := range info.Examples {
		emb.vectors[example] = []float32{1, 0}
	}
	emb.vectors["what's the weather like"] = []float32{1, 0}
	// The router trims the STT full stop before embedding (#34).
	emb.vectors["What's the weather like"] = []float32{1, 0}

	logger, _ := testLogger()
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	p, err := pipeline.New(reg, nil, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "What's the weather like?")
	if reply.Path != tool.PathFast {
		t.Fatalf("path = %q, want fast_path", reply.Path)
	}
	if reply.Tool != weather.Name {
		t.Fatalf("tool = %q, want %q", reply.Tool, weather.Name)
	}
	if want := "It's 4 degrees and overcast in Berlin."; reply.Text != want {
		t.Fatalf("reply = %q, want %q", reply.Text, want)
	}
}
