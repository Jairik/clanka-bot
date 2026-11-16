package tool_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// liarOut is the old structural hook: a result value whose method claims public
// provenance. Under the removed interface it could lower a LocalOnlyYes Tool's
// flag with no registration-time declaration. With the declared field the
// result has no say, so this test fails on the commit that shipped the hook.
type liarOut struct {
	Text string `json:"text"`
}

func (liarOut) LocalOnlyProvenance() (bool, bool) { return false, true }

func TestResultCannotLowerLocalOnlyWithoutDeclaredProvenance(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[struct{}, liarOut]{
		Name:        "test.private",
		Description: "A Local-only Tool whose result claims to be public.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (liarOut, error) {
			return liarOut{Text: "my SSN is 123-45-6789"}, nil
		},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := reg.Execute(context.Background(), "test.private", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !reg.Provenance("test.private", result) {
		t.Fatal("a result's LocalOnlyProvenance method lowered a LocalOnlyYes flag without a declared opt-in")
	}
}

func TestDeclaredProvenanceOverridesStaticOnlyWhenKnown(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	declared := tool.Tool[struct{}, string]{
		Name:        "test.replay",
		Description: "Replays text with its own provenance.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (string, error) {
			return "public text", nil
		},
		Provenance: func(out string) (bool, bool) { return out == "private text", true },
	}
	if err := tool.Add(reg, declared); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if reg.Provenance("test.replay", "public text") {
		t.Fatal("declared hook reported Local-only for public replay text")
	}
	if !reg.Provenance("test.replay", "private text") {
		t.Fatal("declared hook did not report Local-only for private replay text")
	}
	// ok=false means the result reports no provenance: the static flag stands.
	declared.Provenance = func(string) (bool, bool) { return false, false }
	reg2 := tool.NewRegistry(time.Second)
	if err := tool.Add(reg2, declared); err != nil {
		t.Fatalf("Add with no-op hook: %v", err)
	}
	if !reg2.Provenance("test.replay", "public text") {
		t.Fatal("ok=false must fall back to the static LocalOnlyYes flag")
	}
}

func TestAddRejectsProvenanceOnPublicTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	err := tool.Add(reg, tool.Tool[struct{}, string]{
		Name:        "test.public",
		Description: "A public Tool declaring a provenance hook.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			return "text", nil
		},
		Provenance: func(string) (bool, bool) { return true, true },
	})
	if err == nil {
		t.Fatal("Add accepted a Provenance hook on a LocalOnlyNo Tool")
	}
}

type pointerOut struct {
	Text string `json:"text"`
}

func TestNilPointerResultDoesNotPanic(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[struct{}, *pointerOut]{
		Name:        "test.nil",
		Description: "Returns a typed nil result.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (*pointerOut, error) {
			return nil, nil
		},
		Provenance: func(out *pointerOut) (bool, bool) {
			// Would panic if the registry called the hook on a nil receiver.
			return out.Text == "", true
		},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := reg.Execute(context.Background(), "test.nil", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The nil result reports no provenance, so the static LocalOnlyYes stands.
	if !reg.Provenance("test.nil", result) {
		t.Fatal("nil pointer result did not fall back to the static flag")
	}
}

func TestProvenanceOfUnknownToolIsConservative(t *testing.T) {
	if tool.NewRegistry(time.Second).Provenance("missing", "text") {
		t.Fatal("an unknown Tool was reported as Local-only")
	}
}
