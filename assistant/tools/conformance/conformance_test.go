package conformance

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// stubModule is a deliberately broken Tool module for the contract failure
// cases. A well-formed module comes from the real module packages.
type stubModule struct {
	name            string
	register        func(*tool.Registry) error
	samples         map[string]any
	requiredSecrets []string
}

func (m stubModule) Name() string { return m.name }

func (m stubModule) Register(r *tool.Registry, _ tool.Deps) error { return m.register(r) }

func (m stubModule) RequiredSecrets() []string { return m.requiredSecrets }

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

// TestFastPathToolWithoutExamplesFails proves an ordinary Tool must either
// complete the Fast path contract or say AgentOnly deliberately.
func TestFastPathToolWithoutExamplesFails(t *testing.T) {
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.no_examples",
				Description: "A Tool with no route at all.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
				Speak:       func(struct{}) string { return "Hello." },
			})
		},
		samples: map[string]any{"conformance.no_examples": struct{}{}},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, "is not on the semantic Fast path and is not declared AgentOnly")
}

// TestAgentOnlyToolPassesStaticContract proves the explicit Agent-only
// declaration is a complete choice: no Examples, no Extract and no Speak.
func TestAgentOnlyToolPassesStaticContract(t *testing.T) {
	module := stubModule{
		name: "conformance_agent_only",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.agent_only",
				Description: "A Tool the Agent calls; the Fast path never sees it.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				AgentOnly:   true,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
			})
		},
	}
	problems := inspect(module).problems
	if len(problems) != 0 {
		t.Fatalf("Agent-only module problems = %v, want none", problems)
	}
}

// TestAgentOnlyWithExamplesFails proves the contract catches a half-added
// Agent-only Tool before the semantic router can index it.
func TestAgentOnlyWithExamplesFails(t *testing.T) {
	module := stubModule{
		name: "conformance_broken",
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.agent_only_examples",
				Description: "A Tool that contradicts its Agent-only declaration.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				AgentOnly:   true,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
				Examples:    []string{"look it up"},
			})
		},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, "AgentOnly must not declare Examples")
}

// TestBadRequiredSecretsFails proves the module contract rejects an unnamed or
// repeated secret before a module can register with a malformed declaration.
func TestBadRequiredSecretsFails(t *testing.T) {
	module := stubModule{
		name:            "conformance_broken",
		requiredSecrets: []string{"", " SHARED_KEY", "SHARED_KEY"},
		register: func(r *tool.Registry) error {
			return tool.Add(r, tool.Tool[struct{}, struct{}]{
				Name:        "conformance.bad_secrets",
				Description: "A Tool with malformed secret metadata.",
				Permission:  tool.PermissionRead,
				LocalOnly:   tool.LocalOnlyNo,
				Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
			})
		},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, "RequiredSecrets[0] is empty")
	wantProblem(t, problems, `RequiredSecrets[1] " SHARED_KEY" has surrounding whitespace`)
	wantProblem(t, problems, `RequiredSecrets repeats "SHARED_KEY"`)
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

// TestBadIntentDeclarationFails proves the module contract rejects an intent
// outside the fixed vocabulary and a repeated declaration.
func TestBadIntentDeclarationFails(t *testing.T) {
	module := intentStub{
		stubModule: stubModule{
			name: "conformance_bad_intents",
			register: func(r *tool.Registry) error {
				return tool.Add(r, tool.Tool[struct{}, struct{}]{
					Name:        "conformance.bad_intents",
					Description: "A Tool with malformed Focus metadata.",
					Permission:  tool.PermissionRead,
					LocalOnly:   tool.LocalOnlyNo,
					Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
				})
			},
		},
		intents: []focus.Intent{"stopp", focus.Stop, focus.Stop},
	}
	problems := inspect(module).problems
	wantProblem(t, problems, `Intents[0] "stopp" is not a Focus intent`)
	wantProblem(t, problems, `Intents repeats "stop"`)
}

// intentStub is a stub module that opts into the optional Focus declaration.
type intentStub struct {
	stubModule
	intents []focus.Intent
}

func (m intentStub) Intents() []focus.Intent { return m.intents }

func wantProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return
		}
	}
	t.Fatalf("problems %q do not contain %q", problems, want)
}
