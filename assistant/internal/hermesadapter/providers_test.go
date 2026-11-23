package hermesadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
	"gopkg.in/yaml.v3"
)

// TestHermesRunsGoProviders invokes the actual search and weather handlers with local upstream fixtures.
func TestHermesRunsGoProviders(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch r.URL.Path {
		case "/search":
			if r.URL.Query().Get("q") != "Oslo public transit" || r.URL.Query().Get("format") != "json" {
				t.Errorf("unexpected SearXNG request: %s", r.URL)
			}
			body = `{"results":[{"title":"Ruter","url":"https://example.test/transit","content":"Oslo transit service"}]}`
		case "/geocode":
			body = `{"results":[{"name":"Oslo","latitude":59.91,"longitude":10.75,"timezone":"Europe/Oslo","country":"Norway"}]}`
		case "/forecast":
			body = `{"timezone":"Europe/Oslo",
				"current":{"time":"2026-11-11T13:00","temperature_2m":19,"apparent_temperature":18,"relative_humidity_2m":52,"weather_code":0,"wind_speed_10m":5},
				"daily":{"time":["2026-11-11"],"temperature_2m_max":[20],"temperature_2m_min":[10],
				"precipitation_probability_max":[30],"weather_code":[0],"sunrise":["2026-11-11T07:00"],"sunset":["2026-11-11T17:00"]}}`
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	reg := tool.NewRegistry(2 * time.Second)
	webModule := web.NewModule()
	var cfg yaml.Node
	if err := cfg.Encode(map[string]string{"backend": "searxng", "base_url": server.URL, "timeout": "1s"}); err != nil {
		t.Fatal(err)
	}
	if err := webModule.Configure(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := webModule.Register(reg, tool.Deps{}); err != nil {
		t.Fatal(err)
	}
	weatherDeps := weather.Deps{
		HTTP: server.Client(), GeocodingURL: server.URL + "/geocode", ForecastURL: server.URL + "/forecast",
		FallbackURL: server.URL + "/fallback", Budget: time.Second,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) },
	}
	if err := tool.Add(reg, weather.New(weatherDeps)); err != nil {
		t.Fatal(err)
	}
	if err := tool.Add(reg, weather.NewForecast(weatherDeps)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc, err := settings.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	adapter := hermesadapter.New(hermesadapter.Options{Registry: reg, Grants: f.grants, Settings: svc})
	recipe := hermesadapter.Recipe{
		Name: "transit", Description: "Look up Oslo transit", BaseTool: web.Name, Enabled: true,
		Arguments: map[string]json.RawMessage{"query": json.RawMessage(`"Oslo public transit"`)},
	}
	if _, err := adapter.SaveRecipe(ctx, recipe); err != nil {
		t.Fatal(err)
	}
	recipe.Arguments["max_results"] = json.RawMessage(`1.0`)
	if _, err := adapter.SaveRecipe(ctx, recipe); !errors.Is(err, hermesadapter.ErrInvalidRecipe) {
		t.Fatalf("save with non-integer encoding = %v", err)
	}
	schema, err := adapter.Schema("recipe.transit")
	if err != nil || strings.Contains(string(schema.InputSchema), `"query"`) {
		t.Fatalf("unexpected bound search schema: %s (%v)", schema.InputSchema, err)
	}

	handler := hermesadapter.NewHandler(adapter, testToken)
	for _, tc := range []struct{ name, arguments, want string }{
		{web.Name, `{"query":"Oslo public transit"}`, `"title":"Ruter"`},
		{"recipe.transit", `{}`, `"title":"Ruter"`},
		{weather.Name, `{"city":"Oslo"}`, `"temperature_c":19`},
		{weather.ForecastName, `{"city":"Oslo","day":"today"}`, `"high_c":20`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doHTTP(t, handler, http.MethodPost, "/tools/"+tc.name, `{"arguments":`+tc.arguments+`}`, testToken)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) || strings.Contains(rec.Body.String(), "latitude") {
				t.Fatalf("provider response = %d %s", rec.Code, rec.Body)
			}
		})
	}
}
