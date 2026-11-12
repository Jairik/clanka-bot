package conformance

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// stubModule is a deliberately broken Tool module for the contract failure
// cases. A well-formed module comes from the real module packages.
type stubModule struct {
	name     string
	register func(*tool.Registry) error
	samples  map[string]any
}

func (m stubModule) Name() string { return m.name }

func (m stubModule) Register(r *tool.Registry, _ tool.Deps) error { return m.register(r) }

func (m stubModule) ConformanceSamples() map[string]any {
	if m.samples == nil {
		return map[string]any{}
	}
	return m.samples
}

// TestMissingPermissionFails proves registration rejects a Tool that never
// chose a Permission level.
func TestMissingPermissionFails(t *testing.T) {
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.missing_permission",
				Description: "A deliberately broken Tool.",
				LocalOnly:   tool.LocalOnlyNo,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
			})
		},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, `unknown permission ""`)
}

// TestMissingLocalOnlyFails proves registration rejects a Tool that never
// declared Local-only.
func TestMissingLocalOnlyFails(t *testing.T) {
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.missing_local_only",
				Description: "A deliberately broken Tool.",
				Permission:  tool.PermissionRead,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
			})
		},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, "local-only flag is required")
}

// TestEmptySpeakFails proves a Speak that renders nothing fails the sample
// check with an actionable message.
func TestEmptySpeakFails(t *testing.T) {
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.empty_speak",
				Description: "A deliberately silent Tool.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
				Speak:       func(struct{}) string { return "" },
				Examples:    []string{"say nothing"},
			})
		},
		samples: map[string]any{"conformance.empty_speak": struct{}{}},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, "Speak returned empty text")
}

// TestMissingSampleFails proves a module with Speak must declare the output
// the suite should render.
func TestMissingSampleFails(t *testing.T) {
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.no_sample",
				Description: "A Tool without a conformance sample.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
				Speak:       func(struct{}) string { return "Hello." },
				Examples:    []string{"hello"},
			})
		},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, "add a representative output to ConformanceSamples")
}

// TestMisroutedExampleFails proves an Example that does not reach its own Tool
// on the real semantic Fast path fails clearly. It duplicates time.now's first
// Example, so both Tools score 1.0 and the margin rejects the tie.
func TestMisroutedExampleFails(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv(EmbedURLEnv))
	if endpoint == "" {
		t.Skipf("set %s to the real MiniLM embed service to check Example routing", EmbedURLEnv)
	}
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.misrouted",
				Description: "A Tool whose Example belongs to another Tool.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
				Speak:       func(struct{}) string { return "Misrouted." },
				Examples:    []string{"what time is it"},
			})
		},
		samples: map[string]any{"conformance.misrouted": struct{}{}},
	}

	checked := inspect(module)
	if len(checked.problems) > 0 {
		t.Fatalf("static conformance problems: %v", checked.problems)
	}
	tuning, err := loadShippedTuning()
	if err != nil {
		t.Fatalf("load shipped semantic tuning: %v", err)
	}
	problems, err := routingProblems(checked.registry, checked.tools, endpoint, tuning)
	if err != nil {
		t.Fatalf("semantic router: %v", err)
	}
	wantProblem(t, problems, "must route to its own Tool")
}

func wantProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return
		}
	}
	t.Fatalf("problems %q do not contain %q", problems, want)
}
