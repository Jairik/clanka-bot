// Extract tests pin how the Fast path fills Device: a named saved light wins,
// plural "lights" targets all, and an unspecified light with several saved
// stays on the Agent path.
package lights

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func registeredModule(t *testing.T, names ...string) *tool.Registry {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ss, err := settings.New(ctx, st, core.Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SetJSON(ctx, "lights.enabled", json.RawMessage("true")); err != nil {
		t.Fatal(err)
	}
	devices := make([]core.Device, len(names))
	for i, name := range names {
		devices[i] = core.Device{Address: fmt.Sprintf("AA:BB:CC:DD:EE:%02X", i+1), Name: name, Protocol: "lotus"}
	}
	raw, err := json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := json.Marshal(string(raw))
	if _, err := ss.SetJSON(ctx, "lights.devices", value); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry(5 * time.Second)
	if err := NewModule().Register(reg, tool.Deps{Settings: func() *settings.Service { return ss }}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func extracted(t *testing.T, reg *tool.Registry, text string) (In, bool) {
	t.Helper()
	raw, ok := reg.Extract("lights.power", text)
	if !ok {
		return In{}, false
	}
	var in In
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	return in, true
}

// A saved name in the sentence fills Device with the saved spelling.
func TestExtractFillsANamedLight(t *testing.T) {
	reg := registeredModule(t, "Desk Lamp", "Floor")
	in, ok := extracted(t, reg, "turn the desk lamp off")
	if !ok {
		t.Fatal("a named saved light must stay on the Fast path")
	}
	if in.Device != "Desk Lamp" || in.Value != "off" {
		t.Fatalf("in = %+v, want Desk Lamp/off", in)
	}
}

// Plural "lights" with several saved devices targets all of them.
func TestExtractTargetsAllForPluralLights(t *testing.T) {
	reg := registeredModule(t, "Desk Lamp", "Floor")
	in, ok := extracted(t, reg, "turn the lights off")
	if !ok {
		t.Fatal("plural lights must stay on the Fast path")
	}
	if in.Device != core.AllTarget || in.Value != "off" {
		t.Fatalf("in = %+v, want all/off", in)
	}
}

// A singular "light" with several saved devices is ambiguous, so the Agent
// path decides instead of writing every controller.
func TestExtractLeavesAnUnnamedLightToTheAgent(t *testing.T) {
	reg := registeredModule(t, "Desk Lamp", "Floor")
	if in, ok := extracted(t, reg, "turn off the light"); ok {
		t.Fatalf("in = %+v, want no Fast path extraction", in)
	}
}
