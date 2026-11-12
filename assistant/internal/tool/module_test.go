package tool_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// declaringModule implements the optional SecretDeclarer contract.
type declaringModule struct {
	name    string
	secrets []string
}

func (m declaringModule) Name() string { return m.name }

func (m declaringModule) Register(*tool.Registry, tool.Deps) error { return nil }

func (m declaringModule) RequiredSecrets() []string { return m.secrets }

// plainModule keeps the interface optional.
type plainModule struct{}

func (plainModule) Name() string { return "plain" }

func (plainModule) Register(*tool.Registry, tool.Deps) error { return nil }

func TestSecretNamesCollectsInModuleOrderAndDeduplicates(t *testing.T) {
	got := tool.SecretNames(
		declaringModule{name: "first", secrets: []string{"ALPHA_KEY", "SHARED_KEY"}},
		plainModule{},
		declaringModule{name: "second", secrets: []string{"BETA_KEY", "SHARED_KEY", " BETA_KEY "}},
	)
	want := []string{"ALPHA_KEY", "SHARED_KEY", "BETA_KEY"}
	if len(got) != len(want) {
		t.Fatalf("SecretNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SecretNames = %v, want %v", got, want)
		}
	}
	if names := tool.SecretNames(plainModule{}); len(names) != 0 {
		t.Fatalf("SecretNames without a declarer = %v, want none", names)
	}
}

// TestSecretResolverIsTheSharedSeam pins the interface modules receive.
func TestSecretResolverIsTheSharedSeam(t *testing.T) {
	var resolver tool.SecretResolver = fakeResolver{}
	value, ok := resolver.Get("KEY")
	if !ok || value != "value" {
		t.Fatalf("resolver = %q, %v", value, ok)
	}
	var deps tool.Deps
	deps.Secrets = resolver
	if deps.Secrets == nil {
		t.Fatal("Deps.Secrets is not assignable")
	}
}

type fakeResolver struct{}

func (fakeResolver) Get(string) (string, bool) { return "value", true }
