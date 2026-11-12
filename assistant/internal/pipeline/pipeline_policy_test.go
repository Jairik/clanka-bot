package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// fixedSettings is a test double at the registry Policy seam.
type fixedSettings struct {
	enabled    bool
	permission tool.Permission
}

func (s fixedSettings) Effective(string, tool.Permission) tool.ToolSettings {
	return tool.ToolSettings{Enabled: s.enabled, Permission: s.permission}
}

func TestLivePermissionAndDisableOverridesOnTheControlPhrasePath(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.Local)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	phrases := map[string]string{"what time is it": clock.Name}
	p, err := pipeline.New(reg, phrases, nil, pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, nil)))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	req := tool.Request{ID: "test", Path: tool.PathFallthrough, Device: tool.DevicePi}

	// Raise the declared read Tool to confirm after the pipeline exists, as a
	// stored override does mid-run.
	reg.SetPolicy(fixedSettings{enabled: true, permission: tool.PermissionConfirm})
	reply := p.Handle(context.Background(), req, "what time is it")
	if reply.Pending == nil || reply.Pending.Tool != clock.Name {
		t.Fatalf("raised Permission reply = %+v, want an armed Confirmation", reply)
	}
	if strings.TrimSpace(reply.Text) == "" {
		t.Fatal("raised Permission reply has no question text")
	}
	if _, ok := reg.Pending(); !ok {
		t.Fatal("the registry has no pending Confirmation")
	}

	// The following spoken yes runs the Tool through the existing resolver.
	reply = p.Handle(context.Background(), req, "yes")
	if reply.Path != tool.PathConfirmation || reply.Text != "It's 3:04 PM." {
		t.Fatalf("yes reply = %+v, want the confirmed time", reply)
	}
	if _, ok := reg.Pending(); ok {
		t.Fatal("the pending Confirmation survived the yes")
	}

	// Disabling applies to the next request without rebuilding the pipeline.
	reg.SetPolicy(fixedSettings{enabled: false, permission: tool.PermissionRead})
	reply = p.Handle(context.Background(), req, "what time is it")
	if reply.Text != tool.DisabledMessage {
		t.Fatalf("disabled reply = %q, want %q", reply.Text, tool.DisabledMessage)
	}

	// Clearing the policy restores the code declaration.
	reg.SetPolicy(nil)
	reply = p.Handle(context.Background(), req, "what time is it")
	if reply.Text != "It's 3:04 PM." {
		t.Fatalf("restored reply = %q, want the time", reply.Text)
	}
}
