package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
	"gopkg.in/yaml.v3"
)

// stubControls satisfies the audio seam without touching a device.
type stubControls struct {
	delta int
}

func (s *stubControls) SetVolume(context.Context, int) error { return nil }
func (s *stubControls) AdjustVolume(_ context.Context, delta int) error {
	s.delta = delta
	return nil
}
func (s *stubControls) Stop(context.Context) error { return nil }

// recorder collects spoken alerts.
type recorder struct {
	texts chan string
}

func (r *recorder) SayText(_ context.Context, text string) error {
	r.texts <- text
	return nil
}

// sections loads a config file and returns its tools.<name> blocks, proving
// the module list reads the same config.yaml the process does.
func sections(t *testing.T, body string) map[string]yaml.Node {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg.Tools.Sections
}

// register wires every module in the one module list with the given sections.
func register(t *testing.T, deps tool.Deps, sections map[string]yaml.Node) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := tools.NewSet().RegisterAll(reg, deps, sections); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	return reg
}

func TestModuleListRegistersPrototypeTools(t *testing.T) {
	reg := register(t, tool.Deps{Audio: &stubControls{}}, nil)
	for _, name := range []string{
		clock.Name, clock.DateName,
		timer.SetName, timer.CancelName, timer.ListName,
		audiocontrol.VolumeName, audiocontrol.StopName,
	} {
		if _, ok := reg.Info(name); !ok {
			t.Errorf("%s is not registered", name)
		}
	}
	info, ok := reg.Info(weather.Name)
	if !ok {
		t.Fatalf("%s is not registered", weather.Name)
	}
	if info.LocalOnly || info.Permission != tool.PermissionRead || !info.HasSpeak || len(info.Examples) == 0 {
		t.Fatalf("weather.current metadata = %+v, want a non-local read with Speak and Examples", info)
	}
}

func TestModuleDefaultsApplyWithoutASection(t *testing.T) {
	controls := &stubControls{}
	reg := register(t, tool.Deps{Audio: controls}, nil)

	args, ok := reg.Extract(audiocontrol.VolumeName, "louder")
	if !ok {
		t.Fatal("Extract failed")
	}
	if _, err := reg.Execute(context.Background(), audiocontrol.VolumeName, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.delta != audiocontrol.StepPercent {
		t.Fatalf("delta = %d, want the code default %d", controls.delta, audiocontrol.StepPercent)
	}
}

func TestAudioModuleReadsItsConfigSection(t *testing.T) {
	controls := &stubControls{}
	reg := register(t, tool.Deps{Audio: controls}, sections(t, `
tools:
  audio:
    step_percent: 25
`))

	args, ok := reg.Extract(audiocontrol.VolumeName, "louder")
	if !ok {
		t.Fatal("Extract failed")
	}
	if _, err := reg.Execute(context.Background(), audiocontrol.VolumeName, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controls.delta != 25 {
		t.Fatalf("delta = %d, want 25 from tools.audio.step_percent", controls.delta)
	}
}

func TestTimerModuleReadsItsConfigSection(t *testing.T) {
	rec := &recorder{texts: make(chan string, 1)}
	reg := register(t, tool.Deps{Speaker: rec}, sections(t, `
tools:
  timer:
    alert_text: "Time is up."
`))

	if _, err := reg.Execute(context.Background(), timer.SetName, json.RawMessage(`{"duration_seconds":1}`)); err != nil {
		t.Fatalf("Execute timer.set: %v", err)
	}
	select {
	case text := <-rec.texts:
		if text != "Time is up." {
			t.Fatalf("alert = %q, want the configured text", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire an alert")
	}
}

func TestWeatherModuleReadsItsConfigSection(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41}]}`)
	}))
	t.Cleanup(geo.Close)
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"current":{"temperature_2m":8.4,"apparent_temperature":6.1,
			"relative_humidity_2m":70,"weather_code":2,"wind_speed_10m":9.2}}`)
	}))
	t.Cleanup(forecast.Close)

	reg := register(t, tool.Deps{
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin", Region: "Berlin"}, true
		},
	}, sections(t, fmt.Sprintf(`
tools:
  weather:
    geocoding_url: %q
    forecast_url: %q
`, geo.URL, forecast.URL)))

	result, err := reg.Execute(context.Background(), weather.Name, nil)
	if err != nil {
		t.Fatalf("Execute weather.current: %v", err)
	}
	out, ok := result.(weather.Out)
	if !ok {
		t.Fatalf("result is %T, want weather.Out", result)
	}
	if out.City != "Berlin" || out.TemperatureC != 8.4 {
		t.Fatalf("weather.Out = %+v, want the configured fake upstream's Berlin reading", out)
	}
}

func TestWebModuleReadsItsConfigSection(t *testing.T) {
	var searchPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searchPath = r.URL.Path
		fmt.Fprint(w, `{"results":[{"title":"Configured hit","url":"https://example.test/one","content":"from the configured backend"}]}`)
	}))
	t.Cleanup(server.Close)

	reg := register(t, tool.Deps{}, sections(t, fmt.Sprintf(`
tools:
  web:
    backend: searxng
    base_url: %q
    timeout: 1s
    results: 3
    max_results: 6
`, server.URL)))

	result, err := reg.Execute(context.Background(), web.Name, json.RawMessage(`{"query":"configured"}`))
	if err != nil {
		t.Fatalf("Execute %s: %v", web.Name, err)
	}
	out, ok := result.(web.Out)
	if !ok {
		t.Fatalf("result is %T, want web.Out", result)
	}
	if searchPath != "/search" {
		t.Fatalf("search path = %q, want /search", searchPath)
	}
	if len(out.Results) != 1 || out.Results[0].Title != "Configured hit" {
		t.Fatalf("web.Out = %+v, want the configured fake backend's hit", out)
	}
	info, ok := reg.Info(web.Name)
	if !ok || !info.AgentOnly || info.LocalOnly {
		t.Fatalf("web.search metadata = %+v, want AgentOnly and not Local-only", info)
	}
}

func TestModuleConfigRejectsUnknownModule(t *testing.T) {
	err := tools.NewSet().RegisterAll(tool.NewRegistry(time.Second), tool.Deps{}, sections(t, `
tools:
  bogus: {}
`))
	if err == nil || !strings.Contains(err.Error(), "tools.bogus") {
		t.Fatalf("RegisterAll error = %v, want tools.bogus rejected", err)
	}
}

func TestModuleConfigRejectsSettingsForASettinglessModule(t *testing.T) {
	err := tools.NewSet().RegisterAll(tool.NewRegistry(time.Second), tool.Deps{}, sections(t, `
tools:
  clock:
    format: 24h
`))
	if err == nil || !strings.Contains(err.Error(), "tools.clock") {
		t.Fatalf("RegisterAll error = %v, want tools.clock settings rejected", err)
	}
}

func TestModuleConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"audio step out of range", "tools:\n  audio:\n    step_percent: 101\n"},
		{"empty timer alert", "tools:\n  timer:\n    alert_text: \"  \"\n"},
		{"zero weather timeout", "tools:\n  weather:\n    timeout: 0s\n"},
		{"empty weather url", "tools:\n  weather:\n    forecast_url: \"\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tools.NewSet().RegisterAll(tool.NewRegistry(time.Second), tool.Deps{}, sections(t, tt.body))
			if err == nil {
				t.Fatal("RegisterAll succeeded, want a config error")
			}
		})
	}
}

func TestShippedConfigWiresEveryModule(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "config.yaml"))
	if err != nil {
		t.Fatalf("load shipped config: %v", err)
	}
	reg := tool.NewRegistry(time.Second)
	if err := tools.NewSet().RegisterAll(reg, tool.Deps{Audio: &stubControls{}}, cfg.Tools.Sections); err != nil {
		t.Fatalf("RegisterAll with the shipped config: %v", err)
	}
	if len(reg.Names()) == 0 {
		t.Fatal("the shipped config registered no Tools")
	}
}

func TestHealthReportsEachModuleCheck(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41}]}`)
	}))
	t.Cleanup(geo.Close)
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"current":{"temperature_2m":8.4,"weather_code":2}}`)
	}))
	t.Cleanup(forecast.Close)

	set := tools.NewSet()
	deps := tool.Deps{
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin"}, true
		},
	}
	if err := set.RegisterAll(tool.NewRegistry(time.Second), deps, sections(t, fmt.Sprintf(`
tools:
  weather:
    geocoding_url: %q
    forecast_url: %q
`, geo.URL, forecast.URL))); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	results := set.Health(context.Background())
	if len(results) == 0 {
		t.Fatal("Health returned no module checks")
	}
	for _, result := range results {
		if result.Module == "weather" {
			if result.Err != nil {
				t.Fatalf("weather health = %v, want nil against the fake upstream", result.Err)
			}
			return
		}
	}
	t.Fatal("Health returned no weather check")
}

func TestHealthReportsAWeatherUpstreamFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)

	set := tools.NewSet()
	deps := tool.Deps{
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin"}, true
		},
	}
	if err := set.RegisterAll(tool.NewRegistry(time.Second), deps, sections(t, fmt.Sprintf(`
tools:
  weather:
    geocoding_url: %q
    forecast_url: %q
`, bad.URL, bad.URL))); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	for _, result := range set.Health(context.Background()) {
		if result.Module == "weather" {
			if result.Err == nil {
				t.Fatal("weather health = nil, want the upstream failure")
			}
			return
		}
	}
	t.Fatal("Health returned no weather check")
}

// TestFocusIntentsListsTheModuleDeclarations keeps the startup log honest: the
// one module list reports the shared intents each module can handle.
func TestFocusIntentsListsTheModuleDeclarations(t *testing.T) {
	got := tools.NewSet().FocusIntents()
	want := []string{
		"timer:stop", "timer:cancel", "timer:snooze",
		"alarm:stop", "alarm:cancel", "alarm:snooze",
	}
	if len(got) != len(want) {
		t.Fatalf("FocusIntents() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FocusIntents()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
