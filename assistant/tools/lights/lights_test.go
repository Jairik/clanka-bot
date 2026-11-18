// Extract and handler tests pin how the Fast path fills Device and how a
// general command reaches both transports: a named saved light wins, plural
// "lights" targets the whole group, and an unspecified light with several saved
// stays on the Agent path.
package lights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lighting"
	core "github.com/Jairik/clanka-bot/assistant/internal/lights"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func registeredModule(t *testing.T, names ...string) (*Module, *tool.Registry) {
	t.Helper()
	m, reg, _ := registeredModuleWith(t, true, names...)
	return m, reg
}

// registeredModuleWith also reports the settings service, so a test can opt the
// feature out after registration.
func registeredModuleWith(t *testing.T, enabled bool, names ...string) (*Module, *tool.Registry, *settings.Service) {
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
	if _, err := ss.SetJSON(ctx, "lights.enabled", json.RawMessage(fmt.Sprintf("%t", enabled))); err != nil {
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
	m := NewModule()
	reg := tool.NewRegistry(5 * time.Second)
	if err := m.Register(reg, tool.Deps{Settings: func() *settings.Service { return ss }}); err != nil {
		t.Fatal(err)
	}
	return m, reg, ss
}

// fakePC is a scripted PC transport. It records the operations a group command
// sends so a test can prove the control itself and the skip cases.
type fakePC struct {
	claim  bool
	result lighting.Result
	mu     sync.Mutex
	ops    []string
	values []string
}

func (f *fakePC) Targets(string) bool { return f.claim }

func (f *fakePC) ApplyEvery(_ context.Context, op, value string) lighting.Result {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.values = append(f.values, value)
	f.mu.Unlock()
	return f.result
}

func (f *fakePC) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ops)
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
	_, reg := registeredModule(t, "Desk Lamp", "Floor")
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
	_, reg := registeredModule(t, "Desk Lamp", "Floor")
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
	_, reg := registeredModule(t, "Desk Lamp", "Floor")
	if in, ok := extracted(t, reg, "turn off the light"); ok {
		t.Fatalf("in = %+v, want no Fast path extraction", in)
	}
}

// One saved light means "the light" names it; it must not mean the whole group.
func TestExtractNamesTheOnlySavedLight(t *testing.T) {
	_, reg := registeredModule(t, "Desk Lamp")
	in, ok := extracted(t, reg, "turn off the light")
	if !ok {
		t.Fatal("a single saved light is the only possible target")
	}
	if in.Device != "Desk Lamp" {
		t.Fatalf("device = %q, want the saved name so a singular light skips the PC bar", in.Device)
	}
}

// A saved controller literally called "Light Bar" keeps its own commands: the
// literal-name pass runs before the PC alias bail.
func TestExtractLetsASavedNameBeatThePCAlias(t *testing.T) {
	m, reg := registeredModule(t, "Light Bar")
	m.PC = &fakePC{claim: true}
	in, ok := extracted(t, reg, "turn off the light bar")
	if !ok {
		t.Fatal("a saved name must win over the PC alias")
	}
	if in.Device != "Light Bar" {
		t.Fatalf("device = %q, want the saved controller", in.Device)
	}
}

// A command that names the PC light stays off the BLE path, plural included:
// "pc lights" must not mean every saved controller.
func TestExtractLeavesPCTargetsToTheMonsterTools(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	m.PC = &fakePC{claim: true}
	for _, text := range []string{"turn off the pc light", "turn off the pc lights", "switch on the monitor light", "power off the light bar"} {
		if in, ok := extracted(t, reg, text); ok {
			t.Errorf("%q extracted as %+v, want the Monster path", text, in)
		}
	}
}

// runBLE makes the Bluetooth transport answer without hardware. fail names the
// addresses that do not answer.
func runBLE(m *Module, fail ...string) *[]string {
	var mu sync.Mutex
	written := &[]string{}
	m.Service.Run = func(_ context.Context, args ...string) ([]byte, error) {
		for _, address := range fail {
			if args[0] == address {
				return nil, errors.New("device disconnected")
			}
		}
		mu.Lock()
		*written = append(*written, args[0])
		mu.Unlock()
		return nil, nil
	}
	return written
}

// execPower runs lights.power through the registry so schema validation and the
// ToolError mapping are exercised, then reports the reply or the spoken error.
func execPower(ctx context.Context, t *testing.T, reg *tool.Registry, name string, in In) (string, error) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reg.Execute(ctx, "lights."+name, body)
	if err != nil {
		return "", err
	}
	return out.(Out).Reply, nil
}

// A general power command sets the saved controllers and the PC light.
func TestGroupPowerReachesBothTransports(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp", "Floor")
	written := runBLE(m)
	pc := &fakePC{result: lighting.Result{Applied: 1, Total: 1}}
	m.PC = pc

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Light command sent." {
		t.Fatalf("reply = %q", reply)
	}
	if len(*written) != 2 {
		t.Fatalf("BLE writes = %v, want both saved lights", *written)
	}
	if pc.calls() != 1 || pc.ops[0] != "power" || pc.values[0] != "on" {
		t.Fatalf("PC calls = %v/%v", pc.ops, pc.values)
	}
}

// An omitted device is the Agent's "all of them", so it reaches both too.
func TestGroupPowerWithOmittedDeviceReachesBoth(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	runBLE(m)
	pc := &fakePC{result: lighting.Result{Applied: 1, Total: 1}}
	m.PC = pc

	if _, err := execPower(context.Background(), t, reg, "power", In{Value: "off"}); err != nil {
		t.Fatal(err)
	}
	if pc.calls() != 1 {
		t.Fatalf("PC calls = %d, want 1", pc.calls())
	}
}

// A named saved light is that controller alone.
func TestNamedLightDoesNotReachThePC(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp", "Floor")
	runBLE(m)
	pc := &fakePC{result: lighting.Result{Applied: 1, Total: 1}}
	m.PC = pc

	if _, err := execPower(context.Background(), t, reg, "power", In{Device: "Desk Lamp", Value: "on"}); err != nil {
		t.Fatal(err)
	}
	if pc.calls() != 0 {
		t.Fatalf("PC calls = %d, want 0 for a named controller", pc.calls())
	}
}

// A mode is Bluetooth-only: the bar has no mode property.
func TestModeNeverReachesThePC(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	runBLE(m)
	pc := &fakePC{result: lighting.Result{Applied: 1, Total: 1}}
	m.PC = pc

	if _, err := execPower(context.Background(), t, reg, "mode", In{Device: core.AllTarget, Value: "rainbow"}); err != nil {
		t.Fatal(err)
	}
	if pc.calls() != 0 {
		t.Fatalf("PC calls = %d, want 0 for a mode", pc.calls())
	}
}

// Without a Monster account the PC side is skipped silently: the common
// installation must not hear about a light it never set up.
func TestUnconfiguredPCSideIsSkipped(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	runBLE(m)
	m.PC = &fakePC{result: lighting.Result{}}

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Light command sent." {
		t.Fatalf("reply = %q, want no mention of the PC light", reply)
	}
}

// A disabled feature still lets the PC light answer a general command.
func TestDisabledBLEDoesNotBlockThePC(t *testing.T) {
	m, reg, _ := registeredModuleWith(t, false, "Desk Lamp")
	m.Service.Run = func(context.Context, ...string) ([]byte, error) {
		t.Error("a disabled BLE transport must not be written")
		return nil, nil
	}
	m.PC = &fakePC{result: lighting.Result{Applied: 1, Total: 1}}

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "PC light command sent." {
		t.Fatalf("reply = %q", reply)
	}
}

// A cloud the account owns but cannot reach is said out loud.
func TestPCFailureIsReportedBesideARoomSuccess(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	runBLE(m)
	m.PC = &fakePC{result: lighting.Result{Total: 1, Err: errors.New("Monster did not answer")}}

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Light command sent. The PC light did not answer." {
		t.Fatalf("reply = %q", reply)
	}
}

// A dead controller is reported beside a PC success.
func TestRoomFailureIsReportedBesideAPCSuccess(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	runBLE(m, "AA:BB:CC:DD:EE:01")
	m.PC = &fakePC{result: lighting.Result{Applied: 1, Total: 1}}

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Light command sent. No room light answered." {
		t.Fatalf("reply = %q", reply)
	}
}

// A partial room batch reports its count, not a false success.
func TestPartialRoomBatchIsCounted(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp", "Floor")
	runBLE(m, "AA:BB:CC:DD:EE:02")
	m.PC = &fakePC{result: lighting.Result{Applied: 1, Total: 1}}

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Light command sent. 1 of 2 room lights answered." {
		t.Fatalf("reply = %q", reply)
	}
}

// When neither transport answers the command is a spoken failure.
func TestNeitherTransportAnsweringIsAnError(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	runBLE(m, "AA:BB:CC:DD:EE:01")
	m.PC = &fakePC{result: lighting.Result{Total: 1, Err: errors.New("Monster did not answer in time")}}

	_, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) || toolErr.Say != "Neither the room lights nor the PC light answered." {
		t.Fatalf("err = %v, want the combined spoken failure", err)
	}
}

// With no saved controller and no PC account there is nothing to command.
func TestGroupWithNoLightsIsSpoken(t *testing.T) {
	m, reg, _ := registeredModuleWith(t, false)
	m.PC = &fakePC{result: lighting.Result{}}

	_, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) || !strings.Contains(toolErr.Say, "No lights") {
		t.Fatalf("err = %v, want a spoken 'No lights' error", err)
	}
}

// A PC-only installation still answers a general command: no BLE controller
// saved, the cloud light on.
func TestPCOnlyGroupCommand(t *testing.T) {
	m, reg, _ := registeredModuleWith(t, false)
	m.PC = &fakePC{result: lighting.Result{Applied: 1, Total: 1}}

	reply, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "PC light command sent." {
		t.Fatalf("reply = %q", reply)
	}
}

// A PC-only failure is the command's own error text, not a generic one.
func TestPCOnlyFailureKeepsTheCause(t *testing.T) {
	m, reg, _ := registeredModuleWith(t, false)
	m.PC = &fakePC{result: lighting.Result{Err: errors.New("Monster did not answer in time")}}

	_, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) || !strings.Contains(toolErr.Say, "did not answer in time") {
		t.Fatalf("err = %v, want the cloud cause", err)
	}
}

// The two transports run at once: a slow PC side does not delay the BLE write
// beyond its own budget, and the reply still composes.
func TestGroupCommandRunsTransportsConcurrently(t *testing.T) {
	m, reg := registeredModule(t, "Desk Lamp")
	release := make(chan struct{})
	written := make(chan struct{}, 1)
	m.Service.Run = func(context.Context, ...string) ([]byte, error) {
		written <- struct{}{}
		return nil, nil
	}
	m.PC = &waitingPC{release: release, result: lighting.Result{Applied: 1, Total: 1}}

	done := make(chan error, 1)
	go func() {
		_, err := execPower(context.Background(), t, reg, "power", In{Device: core.AllTarget, Value: "on"})
		done <- err
	}()
	select {
	case <-written:
		// The BLE write finished while the PC call is still held.
	case <-time.After(2 * time.Second):
		t.Fatal("the BLE write waited for the PC transport")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// waitingPC holds ApplyEvery until the test releases it.
type waitingPC struct {
	release chan struct{}
	result  lighting.Result
}

func (*waitingPC) Targets(string) bool { return false }
func (p *waitingPC) ApplyEvery(context.Context, string, string) lighting.Result {
	<-p.release
	return p.result
}
