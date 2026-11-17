package monster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	core "github.com/Jairik/clanka-bot/assistant/internal/monster"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type setCall struct {
	dsn, name string
	value     any
}

type fakeAPI struct {
	devices    []core.Device
	properties map[string]map[string]any
	err        error
	mu         sync.Mutex
	setCalls   []setCall
}

func (f *fakeAPI) Devices(context.Context, string, string) ([]core.Device, error) {
	return f.devices, f.err
}

func (f *fakeAPI) Properties(_ context.Context, _, _, dsn string) (map[string]any, error) {
	if f.properties != nil {
		return f.properties[dsn], nil
	}
	return nil, nil
}

func (f *fakeAPI) Set(_ context.Context, _, _, dsn, name string, value any) error {
	f.mu.Lock()
	f.setCalls = append(f.setCalls, setCall{dsn: dsn, name: name, value: value})
	f.mu.Unlock()
	return nil
}

func (f *fakeAPI) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setCall(nil), f.setCalls...)
}

func (f *fakeAPI) clearCalls() {
	f.mu.Lock()
	f.setCalls = nil
	f.mu.Unlock()
}

func configured(api core.API) *Module {
	return &Module{Service: core.NewService(core.Options{
		API:         api,
		Credentials: func() (string, string, bool) { return "owner@example.com", "secret", true },
	})}
}

func TestRequiredSecrets(t *testing.T) {
	got := (&Module{}).RequiredSecrets()
	if len(got) != 2 || got[0] != core.SecretEmail || got[1] != core.SecretPassword {
		t.Fatalf("secrets = %v", got)
	}
}

func TestAuxHealthWithoutCredentialsIsNotConfigured(t *testing.T) {
	m := &Module{Service: core.NewService(core.Options{API: &fakeAPI{}})}
	rows := m.AuxHealth(context.Background())
	if len(rows) != 1 || rows[0].State != tool.StatusNotConfigured || !strings.Contains(rows[0].Detail, "MONSTER_EMAIL") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAuxHealthReportsTheAccount(t *testing.T) {
	m := configured(&fakeAPI{devices: []core.Device{{DSN: "DSN1"}, {DSN: "DSN2"}}})
	rows := m.AuxHealth(context.Background())
	if len(rows) != 1 || rows[0].State != tool.StatusOK || rows[0].Detail != "2 lights" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAuxHealthReportsACloudFailure(t *testing.T) {
	m := configured(&fakeAPI{err: core.ErrCredentials})
	rows := m.AuxHealth(context.Background())
	if len(rows) != 1 || rows[0].State != tool.StatusError || !strings.Contains(rows[0].Detail, "rejected") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAuxHealthWithoutAService(t *testing.T) {
	if rows := (&Module{}).AuxHealth(context.Background()); rows != nil {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestRegisterWithoutSecretsLeavesThePageNotConfigured(t *testing.T) {
	m := NewModule()
	if err := m.Register(tool.NewRegistry(0), tool.Deps{}); err != nil {
		t.Fatal(err)
	}
	if m.Service.Configured() {
		t.Fatal("Register with no Secrets resolver must not claim an account")
	}
	if _, ok, detail := m.Service.Health(context.Background()); ok || !strings.Contains(detail, "MONSTER_EMAIL") {
		t.Fatalf("health = %v %q", ok, detail)
	}
}

// toolByName finds a registered tool in the module's tool slice.
func toolByName(m *Module, name string) tool.Tool[In, Out] {
	for _, t := range m.tools() {
		if t.Name == name {
			return t
		}
	}
	panic("tool not found: " + name)
}

// TestToolsWriteExpectedPropertiesModeBeforeValue tests that each Tool writes
// the expected properties, including setting mode before value for mode-based actions.
func TestToolsWriteExpectedPropertiesModeBeforeValue(t *testing.T) {
	api := &fakeAPI{
		devices: []core.Device{{DSN: "DSN1", Name: "Duo+"}},
		properties: map[string]map[string]any{
			"DSN1": {
				"mode":         "color",
				"brightness":   40,
				"color_bright": 35,
				"dyn02":        `{"n":"Sunset"}`,
			},
		},
	}
	m := configured(api)
	ctx := context.Background()

	// 1. monster.power: on
	powerTool := toolByName(m, "monster.power")
	api.clearCalls()
	if out, err := powerTool.Handler(ctx, In{Device: "Duo+", Value: "on"}); err != nil || out.Reply != "Done." {
		t.Fatalf("power on: %v, out=%+v", err, out)
	}
	calls := api.calls()
	if len(calls) != 1 || calls[0].name != "power" || calls[0].value != 1 {
		t.Fatalf("power on calls = %+v", calls)
	}

	// monster.power: off
	api.clearCalls()
	if _, err := powerTool.Handler(ctx, In{Device: "Duo+", Value: "off"}); err != nil {
		t.Fatalf("power off: %v", err)
	}
	calls = api.calls()
	if len(calls) != 1 || calls[0].name != "power" || calls[0].value != 0 {
		t.Fatalf("power off calls = %+v", calls)
	}

	// 2. monster.brightness: color mode uses color_bright
	brightTool := toolByName(m, "monster.brightness")
	api.clearCalls()
	if _, err := brightTool.Handler(ctx, In{Device: "Duo+", Value: "75%"}); err != nil {
		t.Fatalf("brightness: %v", err)
	}
	calls = api.calls()
	if len(calls) != 1 || calls[0].name != "color_bright" || calls[0].value != 75 {
		t.Fatalf("brightness calls = %+v", calls)
	}

	// monster.brightness: white mode uses brightness
	api.properties["DSN1"]["mode"] = "white"
	api.clearCalls()
	if _, err := brightTool.Handler(ctx, In{Device: "Duo+", Value: "50"}); err != nil {
		t.Fatalf("brightness white mode: %v", err)
	}
	calls = api.calls()
	if len(calls) != 1 || calls[0].name != "brightness" || calls[0].value != 50 {
		t.Fatalf("brightness white mode calls = %+v", calls)
	}

	// 3. monster.color: writes mode=color before color_select
	colorTool := toolByName(m, "monster.color")
	api.clearCalls()
	if _, err := colorTool.Handler(ctx, In{Device: "Duo+", Value: "blue"}); err != nil {
		t.Fatalf("color: %v", err)
	}
	calls = api.calls()
	if len(calls) != 2 || calls[0].name != "mode" || calls[0].value != "color" || calls[1].name != "color_select" || calls[1].value != 0x0000ff {
		t.Fatalf("color calls = %+v (want mode before color_select)", calls)
	}

	// 4. monster.white: writes mode=white before color_temp
	whiteTool := toolByName(m, "monster.white")
	api.clearCalls()
	if _, err := whiteTool.Handler(ctx, In{Device: "Duo+", Value: "warm"}); err != nil {
		t.Fatalf("white warm: %v", err)
	}
	calls = api.calls()
	if len(calls) != 2 || calls[0].name != "mode" || calls[0].value != "white" || calls[1].name != "color_temp" || calls[1].value != 0 {
		t.Fatalf("white warm calls = %+v (want mode before color_temp)", calls)
	}

	api.clearCalls()
	if _, err := whiteTool.Handler(ctx, In{Device: "Duo+", Value: "daylight"}); err != nil {
		t.Fatalf("white daylight: %v", err)
	}
	calls = api.calls()
	if len(calls) != 2 || calls[0].name != "mode" || calls[0].value != "white" || calls[1].name != "color_temp" || calls[1].value != 100 {
		t.Fatalf("white daylight calls = %+v", calls)
	}

	// 5. monster.scene: writes mode before effect index
	sceneTool := toolByName(m, "monster.scene")
	api.clearCalls()
	if _, err := sceneTool.Handler(ctx, In{Device: "Duo+", Value: "sunset"}); err != nil {
		t.Fatalf("scene: %v", err)
	}
	calls = api.calls()
	if len(calls) != 2 || calls[0].name != "mode" || calls[0].value != "dynamic" || calls[1].name != "dyn_pat" || calls[1].value != 2 {
		t.Fatalf("scene calls = %+v (want mode before dyn_pat)", calls)
	}
}

// TestDeviceNameResolvesCaseInsensitively proves device resolution works case-insensitively
// and allows omitting the device argument when only one device is configured.
func TestDeviceNameResolvesCaseInsensitively(t *testing.T) {
	api := &fakeAPI{devices: []core.Device{{DSN: "DSN1", Name: "Duo+"}}}
	m := configured(api)
	ctx := context.Background()
	powerTool := toolByName(m, "monster.power")

	// Exact match
	api.clearCalls()
	if _, err := powerTool.Handler(ctx, In{Device: "Duo+", Value: "on"}); err != nil {
		t.Fatalf("Duo+ failed: %v", err)
	}

	// Lowercase match
	api.clearCalls()
	if _, err := powerTool.Handler(ctx, In{Device: "duo+", Value: "on"}); err != nil {
		t.Fatalf("duo+ failed: %v", err)
	}

	// Uppercase match
	api.clearCalls()
	if _, err := powerTool.Handler(ctx, In{Device: "DUO+", Value: "on"}); err != nil {
		t.Fatalf("DUO+ failed: %v", err)
	}

	// Omitted device argument with one device
	api.clearCalls()
	if _, err := powerTool.Handler(ctx, In{Device: "", Value: "on"}); err != nil {
		t.Fatalf("omitted device failed: %v", err)
	}
}

// TestUnknownDeviceGivesSpokenRefusal proves that an unresolvable device produces
// a spoken refusal ToolError.
func TestUnknownDeviceGivesSpokenRefusal(t *testing.T) {
	api := &fakeAPI{devices: []core.Device{{DSN: "DSN1", Name: "Duo+"}}}
	m := configured(api)
	ctx := context.Background()
	powerTool := toolByName(m, "monster.power")

	_, err := powerTool.Handler(ctx, In{Device: "Living Room Light", Value: "on"})
	if err == nil {
		t.Fatal("expected error for unknown device")
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %T %v, want *tool.ToolError", err, err)
	}
	if !strings.Contains(toolErr.Say, "Living Room Light") {
		t.Fatalf("toolErr.Say = %q, want mention of requested device", toolErr.Say)
	}
}

// TestUnknownSceneGivesSpokenRefusal proves an unknown scene gives a spoken refusal.
func TestUnknownSceneGivesSpokenRefusal(t *testing.T) {
	api := &fakeAPI{
		devices: []core.Device{{DSN: "DSN1", Name: "Duo+"}},
		properties: map[string]map[string]any{
			"DSN1": {
				"dyn02": `{"n":"Sunset"}`,
			},
		},
	}
	m := configured(api)
	ctx := context.Background()
	sceneTool := toolByName(m, "monster.scene")

	_, err := sceneTool.Handler(ctx, In{Device: "Duo+", Value: "Disco"})
	if err == nil {
		t.Fatal("expected error for unknown scene")
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %T %v, want *tool.ToolError", err, err)
	}
	if !strings.Contains(toolErr.Say, "Disco") {
		t.Fatalf("toolErr.Say = %q, want mention of requested scene", toolErr.Say)
	}
}

// TestMultipleDevicesRequiresDeviceArgument proves omitting device fails when
// more than one device exists on the account.
func TestMultipleDevicesRequiresDeviceArgument(t *testing.T) {
	api := &fakeAPI{devices: []core.Device{
		{DSN: "DSN1", Name: "Light Bar Left"},
		{DSN: "DSN2", Name: "Light Bar Right"},
	}}
	m := configured(api)
	ctx := context.Background()
	powerTool := toolByName(m, "monster.power")

	_, err := powerTool.Handler(ctx, In{Device: "", Value: "on"})
	if err == nil {
		t.Fatal("expected error when multiple devices exist and none specified")
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %T %v, want *tool.ToolError", err, err)
	}
	if !strings.Contains(toolErr.Say, "Specify") {
		t.Fatalf("toolErr.Say = %q, want 'Specify which Monster light...'", toolErr.Say)
	}
}

// TestToolExtract tests that Fast path extraction parses values correctly and ignores questions.
func TestToolExtract(t *testing.T) {
	m := &Module{}

	// monster.power
	powerTool := toolByName(m, "monster.power")
	in, ok := powerTool.Extract("turn the monitor light on")
	if !ok || in.Value != "on" {
		t.Fatalf("extract power on: ok=%v in=%+v", ok, in)
	}
	in, ok = powerTool.Extract("turn off the light bar")
	if !ok || in.Value != "off" {
		t.Fatalf("extract power off: ok=%v in=%+v", ok, in)
	}
	if _, ok := powerTool.Extract("how do I turn the monitor light on"); ok {
		t.Fatal("expected question to be rejected by Extract")
	}

	// Plain "turn the lights on" must stay off monster tools
	if _, ok := powerTool.Extract("turn the lights on"); ok {
		t.Fatal("plain 'turn the lights on' must not match monster.power")
	}

	// monster.brightness
	brightTool := toolByName(m, "monster.brightness")
	in, ok = brightTool.Extract("set light bar level to 50 percent")
	if !ok || in.Value != "50" {
		t.Fatalf("extract brightness: ok=%v in=%+v", ok, in)
	}

	// monster.color
	colorTool := toolByName(m, "monster.color")
	in, ok = colorTool.Extract("make the monitor light red")
	if !ok || in.Value != "red" {
		t.Fatalf("extract color: ok=%v in=%+v", ok, in)
	}

	// monster.white
	whiteTool := toolByName(m, "monster.white")
	in, ok = whiteTool.Extract("set the monitor light to warm white")
	if !ok || in.Value != "warm" {
		t.Fatalf("extract white: ok=%v in=%+v", ok, in)
	}
	in, ok = whiteTool.Extract("set light bar white to 4000 kelvin")
	if !ok || in.Value != "4000" {
		t.Fatalf("extract kelvin: ok=%v in=%+v", ok, in)
	}

	// monster.scene
	sceneTool := toolByName(m, "monster.scene")
	in, ok = sceneTool.Extract("set monitor light to reading scene")
	if !ok || in.Value != "reading" {
		t.Fatalf("extract scene: ok=%v in=%+v", ok, in)
	}
}
