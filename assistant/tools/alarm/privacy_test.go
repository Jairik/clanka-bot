package alarm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// newUpdateRegistry builds the real alarm module over an in-memory Manager.
func newUpdateRegistry(t *testing.T) (*tool.Registry, *alarm.Manager) {
	t.Helper()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newManager(t, clock, nil, nil, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg, manager
}

// TestUpdateIsLocalOnly mirrors TestRemainingIsLocalOnly: alarm.update returns
// the whole stored Alarm, including a label a time/id/no-selector match never
// named, so the Agent path must not see or call it (#322).
func TestUpdateIsLocalOnly(t *testing.T) {
	reg, manager := newUpdateRegistry(t)
	if got := alarm.NewUpdate(manager).LocalOnly; got != tool.LocalOnlyYes {
		t.Fatalf("alarm.update LocalOnly = %d, want LocalOnlyYes", got)
	}
	info, ok := reg.Info(alarm.UpdateName)
	if !ok {
		t.Fatal("alarm.update not registered")
	}
	if !info.LocalOnly {
		t.Fatal("alarm.update is not LocalOnlyYes")
	}
	// Local-only only hides the Tool from the Agent/MCP surface; the local
	// Examples and Extract must still keep it on the semantic Fast path.
	if !pipeline.FastPathEligible(info) {
		t.Error("alarm.update is not Fast path eligible; a Local-only Tool must still route locally")
	}
}

// TestUpdateIsNotOfferedToTheAgent checks the real request builder, not just
// the declaration: the model never receives alarm.update as a callable Tool,
// while the label-free alarm.set stays available.
func TestUpdateIsNotOfferedToTheAgent(t *testing.T) {
	reg, _ := newUpdateRegistry(t)
	update, ok := reg.Info(alarm.UpdateName)
	if !ok {
		t.Fatal("alarm.update not registered")
	}
	set, ok := reg.Info(alarm.SetName)
	if !ok {
		t.Fatal("alarm.set not registered")
	}
	seen := make(map[string]bool)
	for _, definition := range agent.NewPromptBuilder("").Tools(reg) {
		if definition.OfFunction != nil {
			seen[definition.OfFunction.Function.Name] = true
		}
	}
	if seen[update.WireName] {
		t.Error("the Agent tool list offers the Local-only alarm.update")
	}
	if !seen[set.WireName] {
		t.Error("the Agent tool list is missing alarm.set")
	}
}

// TestUpdateKeepsTheStoredLabelOnThePi documents why the Tool is Local-only:
// a request that selects by time never says the label, but the result retains
// it for the local spoken update.
func TestUpdateKeepsTheStoredLabelOnThePi(t *testing.T) {
	reg, manager := newUpdateRegistry(t)
	if _, err := manager.Set(context.Background(), alarm.SetIn{
		Hour: 7, Schedule: "daily", Label: "medication",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	args, ok := reg.Extract(alarm.UpdateName, "change my 7 am alarm volume to 40")
	if !ok {
		t.Fatal("alarm.update Extract failed")
	}
	result, err := reg.Execute(context.Background(), alarm.UpdateName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(alarm.UpdateOut)
	if out.Matched != 1 || out.Alarm.Label != "medication" ||
		out.Alarm.Volume == nil || *out.Alarm.Volume != 40 {
		t.Fatalf("update = %+v, want the stored label and volume 40", out)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The local spoken result retains the label; Local-only guards its egress.
	if !strings.Contains(string(payload), `"label":"medication"`) {
		t.Fatalf("payload = %s, want the stored label", payload)
	}
}
