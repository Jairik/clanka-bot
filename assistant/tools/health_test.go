package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// The Set owns its module list, so these fakes live in the package to place
// them in one. Each implements only the optional health interfaces it needs,
// exactly as a real module does.
type bareModule string

func (m bareModule) Name() string                             { return string(m) }
func (m bareModule) Register(*tool.Registry, tool.Deps) error { return nil }

type checkerModule struct {
	name  string
	check func(context.Context) error
}

func (m checkerModule) Name() string                             { return m.name }
func (m checkerModule) Register(*tool.Registry, tool.Deps) error { return nil }
func (m checkerModule) Health(ctx context.Context) error         { return m.check(ctx) }

type auxModule struct {
	name string
	aux  func(context.Context) []tool.AuxHealth
}

func (m auxModule) Name() string                             { return m.name }
func (m auxModule) Register(*tool.Registry, tool.Deps) error { return nil }
func (m auxModule) AuxHealth(ctx context.Context) []tool.AuxHealth {
	return m.aux(ctx)
}

type checkedAuxModule struct {
	checkerModule
	aux func(context.Context) []tool.AuxHealth
}

func (m checkedAuxModule) AuxHealth(ctx context.Context) []tool.AuxHealth { return m.aux(ctx) }

func healthByName(results []Health) map[string]Health {
	byName := make(map[string]Health, len(results))
	for _, result := range results {
		byName[result.Module] = result
	}
	return byName
}

// TestHealthIsolatesASlowModule is the #208 regression at the seam that owns the
// checks: a module that misses the snapshot deadline is timed out alone, and
// every module that answered keeps its real state.
func TestHealthIsolatesASlowModule(t *testing.T) {
	set := &Set{modules: []tool.Module{
		bareModule("clock"),
		checkerModule{name: "weather", check: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		checkerModule{name: "audio", check: func(context.Context) error { return nil }},
		checkerModule{name: "github", check: func(context.Context) error { return errors.New("token rejected") }},
	}}

	start := time.Now()
	results := set.Health(withTimeout(t, 25*time.Millisecond))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Health took %s, want the 25ms budget to bound it", elapsed)
	}

	byName := healthByName(results)
	if slow := byName["weather"]; !slow.TimedOut || slow.Err != nil {
		t.Errorf("weather = %+v, want only its own TimedOut flag", slow)
	}
	if audio := byName["audio"]; audio.TimedOut || audio.Err != nil {
		t.Errorf("audio = %+v, want its real ok state", audio)
	}
	if github := byName["github"]; github.TimedOut || github.Err == nil || !strings.Contains(github.Err.Error(), "rejected") {
		t.Errorf("github = %+v, want its real error", github)
	}
	if row, ok := byName["clock"]; ok {
		t.Errorf("clock = %+v, want no row for a module without a check", row)
	}
	want := []string{"weather", "audio", "github"}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %d rows", results, len(want))
	}
	for i, name := range want {
		if results[i].Module != name {
			t.Fatalf("results = %+v, want module order %v", results, want)
		}
	}
}

// TestHealthSurvivesAPanickingCheck keeps one module's panic from taking the
// whole assistant down or hiding the rows that did answer.
func TestHealthSurvivesAPanickingCheck(t *testing.T) {
	set := &Set{modules: []tool.Module{
		auxModule{name: "monster", aux: func(context.Context) []tool.AuxHealth {
			panic("nil service")
		}},
		checkerModule{name: "audio", check: func(context.Context) error { return nil }},
	}}

	results := set.Health(withTimeout(t, time.Second))
	byName := healthByName(results)
	if monster := byName["monster"]; monster.Err == nil || !strings.Contains(monster.Err.Error(), "panicked") {
		t.Errorf("monster = %+v, want a panic error row", monster)
	}
	if audio := byName["audio"]; audio.Err != nil || audio.TimedOut {
		t.Errorf("audio = %+v, want its real ok state", audio)
	}
}

// TestHealthKeepsAModulesAuxRowsAfterItsOwn proves the view's row order and the
// separate capability rows are unchanged by the concurrent pass.
func TestHealthKeepsAModulesAuxRowsAfterItsOwn(t *testing.T) {
	set := &Set{modules: []tool.Module{
		checkedAuxModule{
			checkerModule: checkerModule{name: "github", check: func(context.Context) error { return nil }},
			aux: func(context.Context) []tool.AuxHealth {
				return []tool.AuxHealth{{Name: "github notifications", State: tool.StatusNotConfigured, Detail: "needs a classic token"}}
			},
		},
	}}

	results := set.Health(withTimeout(t, time.Second))
	if len(results) != 2 || results[0].Module != "github" || results[1].Module != "github notifications" {
		t.Fatalf("results = %+v, want the module row then its aux row", results)
	}
	if results[1].Check == nil || results[1].Check.State != tool.StatusNotConfigured {
		t.Errorf("aux = %+v, want its own not-configured state", results[1])
	}
}

func withTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}
