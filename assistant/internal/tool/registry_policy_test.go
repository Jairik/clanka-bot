package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// staticPolicy is a test double at the Registry's public Policy seam.
type staticPolicy struct {
	mu       sync.RWMutex
	settings map[string]tool.ToolSettings
}

func newStaticPolicy() *staticPolicy {
	return &staticPolicy{settings: make(map[string]tool.ToolSettings)}
}

func (p *staticPolicy) Effective(name string, declared tool.Permission) tool.ToolSettings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if setting, ok := p.settings[name]; ok {
		return setting
	}
	return tool.ToolSettings{Enabled: true, Permission: declared}
}

func (p *staticPolicy) set(name string, setting tool.ToolSettings) {
	p.mu.Lock()
	p.settings[name] = setting
	p.mu.Unlock()
}

func TestPolicyAppliesToTheNextCallWithoutReRegistering(t *testing.T) {
	reg := tool.NewRegistry(2 * time.Second)
	if err := tool.Add(reg, echoTool()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	policy := newStaticPolicy()
	reg.SetPolicy(policy)
	args := json.RawMessage(`{"text":"hello"}`)

	policy.set("test.echo", tool.ToolSettings{Enabled: false, Permission: tool.PermissionRead})
	if _, err := reg.Execute(context.Background(), "test.echo", args); !errors.Is(err, tool.ErrToolDisabled) {
		t.Fatalf("Execute with a disabled Tool error = %v, want ErrToolDisabled", err)
	}
	if got := tool.FailureMessage(errFromExecute(t, reg, "test.echo", args)); got != tool.DisabledMessage {
		t.Errorf("FailureMessage = %q, want the disabled message", got)
	}
	info, _ := reg.Info("test.echo")
	if info.Enabled {
		t.Error("Info.Enabled = true, want false after the override")
	}

	// Raising a read Tool to confirm needs a spoken Confirmation.
	policy.set("test.echo", tool.ToolSettings{Enabled: true, Permission: tool.PermissionConfirm})
	if _, err := reg.Execute(context.Background(), "test.echo", args); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("Execute with a raised Tool error = %v, want ErrConfirmationRequired", err)
	}
	info, _ = reg.Info("test.echo")
	if info.Permission != tool.PermissionConfirm {
		t.Errorf("Info.Permission = %q, want the effective confirm", info.Permission)
	}
	if _, err := reg.Propose("test.echo", args); err != nil {
		t.Fatalf("Propose with an effective confirm Tool: %v", err)
	}
	if _, pending, err := reg.ExecutePending(context.Background()); err != nil || pending.Tool != "test.echo" {
		t.Fatalf("ExecutePending = %q, %v", pending.Tool, err)
	}

	// Clearing the policy restores the code-declared metadata.
	reg.SetPolicy(nil)
	info, _ = reg.Info("test.echo")
	if !info.Enabled || info.Permission != tool.PermissionRead {
		t.Errorf("Info after clearing the policy = %+v, want enabled read", info)
	}
	if _, err := reg.Execute(context.Background(), "test.echo", args); err != nil {
		t.Fatalf("Execute after clearing the policy: %v", err)
	}
}

func errFromExecute(t *testing.T, reg *tool.Registry, name string, args json.RawMessage) error {
	t.Helper()
	_, err := reg.Execute(context.Background(), name, args)
	if err == nil {
		t.Fatal("Execute succeeded, want an error")
	}
	return err
}
