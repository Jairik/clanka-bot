package status_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

type fakeLocation struct {
	place location.Location
	known bool
	err   error
}

func (f fakeLocation) Health(context.Context) error { return f.err }
func (f fakeLocation) Current() (location.Location, bool) {
	return f.place, f.known
}

type fakeModules struct {
	names  []string
	health []tools.Health
	block  bool
}

func (f fakeModules) Modules() []tool.Module { return modules(f.names) }
func (f fakeModules) Health(ctx context.Context) []tools.Health {
	if f.block {
		<-ctx.Done()
		return nil
	}
	return f.health
}

// moduleName is a tool.Module with just a name; Module is an interface, so the
// report only needs the name for a module without a check.
type moduleName string

func (m moduleName) Name() string { return string(m) }
func (moduleName) Register(*tool.Registry, tool.Deps) error {
	return nil
}

func modules(names []string) []tool.Module {
	out := make([]tool.Module, 0, len(names))
	for _, name := range names {
		out = append(out, moduleName(name))
	}
	return out
}

func TestStatusReportsServicesLocationAndModules(t *testing.T) {
	source := status.New(status.Options{
		Timeout: time.Second,
		Services: []status.ServiceCheck{
			{Name: "embed", Check: func(context.Context) error { return nil }},
			{Name: "assistant", Check: func(context.Context) error { return errors.New("dial tcp: connection refused") }},
			{Name: "tts"},
		},
		Location: fakeLocation{
			place: location.Location{City: "Bristol", Region: "England", Timezone: "Europe/London"},
			known: true,
		},
		Modules: fakeModules{
			names: []string{"clock", "weather", "audio"},
			health: []tools.Health{
				{Module: "weather", Err: errors.New("upstream failed")},
				{Module: "audio", Err: nil},
			},
		},
		Now: func() time.Time { return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC) },
	})

	report := source.Status(context.Background())
	if got := report.CheckedAt.Format(time.RFC3339); got != "2026-11-11T15:04:00Z" {
		t.Errorf("CheckedAt = %s", got)
	}
	if report.Timeout != time.Second {
		t.Errorf("Timeout = %s", report.Timeout)
	}
	byName := map[string]tool.StatusCheck{}
	for _, check := range report.Services {
		byName[check.Name] = check
	}
	if byName["embed"].State != tool.StatusOK || !strings.Contains(byName["embed"].Detail, "responded in") {
		t.Errorf("embed = %+v", byName["embed"])
	}
	if byName["assistant"].State != tool.StatusError || !strings.Contains(byName["assistant"].Detail, "connection refused") {
		t.Errorf("assistant = %+v", byName["assistant"])
	}
	if byName["tts"].State != tool.StatusNotConfigured {
		t.Errorf("tts = %+v, want not-configured", byName["tts"])
	}
	if report.Location.State != tool.StatusOK || report.Location.Detail != "Bristol, England (Europe/London)" {
		t.Errorf("location = %+v", report.Location)
	}
	modulesByName := map[string]tool.StatusCheck{}
	for _, check := range report.Modules {
		modulesByName[check.Name] = check
	}
	if modulesByName["clock"].State != tool.StatusNoCheck {
		t.Errorf("clock = %+v, want no-check", modulesByName["clock"])
	}
	if modulesByName["weather"].State != tool.StatusError || !strings.Contains(modulesByName["weather"].Detail, "upstream failed") {
		t.Errorf("weather = %+v", modulesByName["weather"])
	}
	if modulesByName["audio"].State != tool.StatusOK {
		t.Errorf("audio = %+v", modulesByName["audio"])
	}
	if report.OK() {
		t.Error("OK() = true with a failing service and module")
	}
}

func TestStatusWithoutAWiredBoundaryIsNotConfigured(t *testing.T) {
	report := status.New(status.Options{Timeout: time.Second}).Status(context.Background())
	if len(report.Services) != 0 {
		t.Errorf("Services = %+v, want none", report.Services)
	}
	if report.Location.State != tool.StatusNotConfigured {
		t.Errorf("location = %+v, want not-configured", report.Location)
	}
	if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusNotConfigured {
		t.Errorf("modules = %+v, want a not-configured row", report.Modules)
	}
	if report.OK() {
		t.Error("OK() = true without a single check; an empty snapshot is not health evidence")
	}
}

func TestStatusBoundsASlowServiceAndModule(t *testing.T) {
	block := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	source := status.New(status.Options{
		Timeout:  25 * time.Millisecond,
		Services: []status.ServiceCheck{{Name: "embed", Check: block}},
		Modules: fakeModules{
			names: []string{"weather"},
			block: true,
		},
	})

	start := time.Now()
	report := source.Status(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Status took %s, want the 25ms budget to bound it", elapsed)
	}
	if report.Services[0].State != tool.StatusTimedOut {
		t.Errorf("embed = %+v, want timed-out", report.Services[0])
	}
	if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusTimedOut {
		t.Errorf("modules = %+v, want a timed-out row", report.Modules)
	}
}

func TestStatusNeverFabricatesAPlace(t *testing.T) {
	report := status.New(status.Options{
		Timeout:  time.Second,
		Location: fakeLocation{known: false},
	}).Status(context.Background())
	if report.Location.State != tool.StatusError || report.Location.Detail != "No place known." {
		t.Fatalf("location = %+v, want the honest no-place error", report.Location)
	}
}

func TestLabels(t *testing.T) {
	cases := map[tool.StatusState]string{
		tool.StatusOK:            "ok",
		tool.StatusError:         "error",
		tool.StatusNotConfigured: "not configured",
		tool.StatusTimedOut:      "timed out",
		tool.StatusNoCheck:       "no check",
	}
	for state, want := range cases {
		if got := status.Label(state); got != want {
			t.Errorf("Label(%q) = %q, want %q", state, got, want)
		}
	}
}
