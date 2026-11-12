package toolpolicy_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
)

type in struct {
	Text string `json:"text"`
}

type out struct {
	Text string `json:"text"`
}

// toolDef describes one test Tool's code declaration.
type toolDef struct {
	name       string
	permission tool.Permission
	localOnly  tool.LocalOnly
}

var testTools = []toolDef{
	{name: "test.read", permission: tool.PermissionRead, localOnly: tool.LocalOnlyNo},
	{name: "test.act", permission: tool.PermissionAct, localOnly: tool.LocalOnlyNo},
	{name: "test.local", permission: tool.PermissionRead, localOnly: tool.LocalOnlyYes},
	{name: "test.confirm", permission: tool.PermissionConfirm, localOnly: tool.LocalOnlyNo},
}

func addTools(t *testing.T, reg *tool.Registry) {
	t.Helper()
	for _, def := range testTools {
		def := def
		err := tool.Add(reg, tool.Tool[in, out]{
			Name:        def.name,
			Description: "Test tool.",
			Permission:  def.permission,
			LocalOnly:   def.localOnly,
			Handler: func(_ context.Context, args in) (out, error) {
				return out{Text: args.Text}, nil
			},
			Speak: func(result out) string { return result.Text },
		})
		if err != nil {
			t.Fatalf("Add %s: %v", def.name, err)
		}
	}
}

// wire builds the registry, settings service and live policy over one temp DB.
func wire(t *testing.T, config toolpolicy.Config) (*tool.Registry, *settings.Service, *toolpolicy.Policy, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	reg := tool.NewRegistry(2 * time.Second)
	addTools(t, reg)
	defs, err := toolpolicy.Definitions(reg, config)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	svc, err := settings.New(context.Background(), st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	policy, err := toolpolicy.New(svc, reg)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}
	return reg, svc, policy, st
}

func boolPtr(value bool) *bool { return &value }

func permissionPtr(value tool.Permission) *tool.Permission { return &value }

func execute(reg *tool.Registry, name string) error {
	_, err := reg.Execute(context.Background(), name, json.RawMessage(`{"text":"hello"}`))
	return err
}

func TestDisableAndReEnableApplyToTheNextCall(t *testing.T) {
	ctx := context.Background()
	reg, _, policy, _ := wire(t, toolpolicy.Config{})

	if err := execute(reg, "test.read"); err != nil {
		t.Fatalf("initial Execute: %v", err)
	}

	state, err := policy.Set(ctx, "test.read", toolpolicy.Update{Enabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if state.Enabled || state.EnabledSource != settings.SourceOverride {
		t.Fatalf("state after disable = %+v", state)
	}
	if err := execute(reg, "test.read"); !errors.Is(err, tool.ErrToolDisabled) {
		t.Fatalf("Execute while disabled = %v, want ErrToolDisabled", err)
	}
	info, _ := reg.Info("test.read")
	if info.Enabled {
		t.Error("registry Info still reports the Tool enabled")
	}

	if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if err := execute(reg, "test.read"); err != nil {
		t.Fatalf("Execute after re-enable: %v", err)
	}
}

func TestPermissionCanOnlyTighten(t *testing.T) {
	ctx := context.Background()
	reg, _, policy, _ := wire(t, toolpolicy.Config{})

	// read -> act is allowed and applies without a restart.
	state, err := policy.Set(ctx, "test.read", toolpolicy.Update{Permission: permissionPtr(tool.PermissionAct)})
	if err != nil {
		t.Fatalf("read -> act: %v", err)
	}
	if state.Permission != tool.PermissionAct {
		t.Fatalf("permission = %q, want act", state.Permission)
	}
	if err := execute(reg, "test.read"); err != nil {
		t.Fatalf("Execute at act: %v", err)
	}

	// act -> confirm needs the registry's Confirmation path.
	state, err = policy.Set(ctx, "test.read", toolpolicy.Update{Permission: permissionPtr(tool.PermissionConfirm)})
	if err != nil {
		t.Fatalf("act -> confirm: %v", err)
	}
	if state.Permission != tool.PermissionConfirm {
		t.Fatalf("permission = %q, want confirm", state.Permission)
	}
	if err := execute(reg, "test.read"); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("Execute at confirm = %v, want ErrConfirmationRequired", err)
	}

	// act -> read would loosen the code-declared act level and is rejected.
	if _, err := policy.Set(ctx, "test.act", toolpolicy.Update{Permission: permissionPtr(tool.PermissionRead)}); err == nil {
		t.Fatal("loosening act -> read succeeded")
	}
	if got := policy.State("test.act").Permission; got != tool.PermissionAct {
		t.Errorf("test.act permission = %q after a rejected loosen, want act", got)
	}
}

func TestConfigLayersSupplyDisabledAndTightenedPermissions(t *testing.T) {
	reg, _, policy, _ := wire(t, toolpolicy.Config{
		Disabled:    []string{"test.read"},
		Permissions: map[string]string{"test.act": "confirm"},
	})

	state := policy.State("test.read")
	if state.Enabled || state.EnabledSource != settings.SourceConfig {
		t.Fatalf("config-disabled test.read = %+v", state)
	}
	if err := execute(reg, "test.read"); !errors.Is(err, tool.ErrToolDisabled) {
		t.Fatalf("Execute a config-disabled Tool = %v, want ErrToolDisabled", err)
	}
	state = policy.State("test.act")
	if state.Permission != tool.PermissionConfirm || state.PermissionSource != settings.SourceConfig {
		t.Fatalf("config-tightened test.act = %+v", state)
	}

	// Reset must fall back to the config layer, not the code default.
	if _, err := policy.Set(context.Background(), "test.read", toolpolicy.Update{Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("override config-disabled Tool: %v", err)
	}
	if err := execute(reg, "test.read"); err != nil {
		t.Fatalf("Execute after the override: %v", err)
	}
	state, err := policy.Reset(context.Background(), "test.read")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if state.Enabled || state.EnabledSource != settings.SourceConfig {
		t.Fatalf("after Reset = %+v, want the config-disabled layer", state)
	}
	if err := execute(reg, "test.read"); !errors.Is(err, tool.ErrToolDisabled) {
		t.Fatalf("Execute after Reset = %v, want ErrToolDisabled", err)
	}
}

func TestDefinitionsRejectUnknownToolsAndLooseningConfig(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	addTools(t, reg)
	tests := []struct {
		name   string
		config toolpolicy.Config
	}{
		{"unknown disabled tool", toolpolicy.Config{Disabled: []string{"test.missing"}}},
		{"unknown permission tool", toolpolicy.Config{Permissions: map[string]string{"test.missing": "act"}}},
		{"loosened permission", toolpolicy.Config{Permissions: map[string]string{"test.act": "read"}}},
		{"invalid permission", toolpolicy.Config{Permissions: map[string]string{"test.read": "admin"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := toolpolicy.Definitions(reg, tt.config); err == nil {
				t.Fatal("Definitions succeeded, want an error")
			}
		})
	}
}

func TestResetRemovesBothOverrides(t *testing.T) {
	ctx := context.Background()
	_, svc, policy, _ := wire(t, toolpolicy.Config{})
	if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{
		Enabled:    boolPtr(false),
		Permission: permissionPtr(tool.PermissionConfirm),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	state, err := policy.Reset(ctx, "test.read")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !state.Enabled || state.Permission != tool.PermissionRead {
		t.Fatalf("after Reset = %+v, want the code declaration", state)
	}
	if state.EnabledSource != settings.SourceDefault || state.PermissionSource != settings.SourceDefault {
		t.Fatalf("after Reset sources = %+v, want default/default", state)
	}
	for _, key := range []string{toolpolicy.EnabledKey("test.read"), toolpolicy.PermissionKey("test.read")} {
		value, ok := svc.Get(key)
		if !ok || value.Source != settings.SourceDefault {
			t.Errorf("%s after Reset = %+v", key, value)
		}
	}
}

func TestLocalOnlyIsNotAnOverridableSetting(t *testing.T) {
	_, svc, policy, _ := wire(t, toolpolicy.Config{})
	state := policy.State("test.local")
	if !state.LocalOnly {
		t.Fatal("test.local is not Local-only in the policy state")
	}
	if _, ok := svc.Get("tools.test.local.local_only"); ok {
		t.Fatal("a local_only setting exists, but the flag must not be overridable")
	}
}

func TestUnknownToolAndEmptyUpdateAreRejected(t *testing.T) {
	ctx := context.Background()
	_, _, policy, _ := wire(t, toolpolicy.Config{})
	if _, err := policy.Set(ctx, "test.missing", toolpolicy.Update{Enabled: boolPtr(false)}); err == nil {
		t.Fatal("Set accepted an unknown Tool")
	}
	if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{}); err == nil {
		t.Fatal("Set accepted an empty update")
	}
	if _, ok := policy.Get("test.missing"); ok {
		t.Fatal("Get found an unknown Tool")
	}
}

func TestDisabledToolIsNotOfferedToTheAgent(t *testing.T) {
	ctx := context.Background()
	reg, _, policy, _ := wire(t, toolpolicy.Config{})
	if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	offered := agent.NewPromptBuilder("").Tools(reg)
	seen := make(map[string]bool, len(offered))
	for _, definition := range offered {
		if definition.OfFunction != nil {
			seen[definition.OfFunction.Function.Name] = true
		}
	}
	if seen["test__read"] {
		t.Error("a disabled Tool is still offered to the model")
	}
	if !seen["test__act"] {
		t.Error("an enabled Tool is missing from the model's Tool list")
	}
}

func TestEffectiveReadsBothOverridesFromOneSnapshot(t *testing.T) {
	ctx := context.Background()
	_, _, policy, _ := wire(t, toolpolicy.Config{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{
				Enabled:    boolPtr(false),
				Permission: permissionPtr(tool.PermissionConfirm),
			}); err != nil {
				t.Errorf("Set disabled+confirm: %v", err)
				return
			}
			if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{
				Enabled:    boolPtr(true),
				Permission: permissionPtr(tool.PermissionRead),
			}); err != nil {
				t.Errorf("Set enabled+read: %v", err)
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}
		setting := policy.Effective("test.read", tool.PermissionRead)
		// Each Set writes both fields in one batch, so a reader must never see
		// the old enabled flag paired with the new Permission.
		if setting.Enabled && setting.Permission != tool.PermissionRead {
			t.Fatalf("mixed snapshot: enabled=%v permission=%q", setting.Enabled, setting.Permission)
		}
		if !setting.Enabled && setting.Permission != tool.PermissionConfirm {
			t.Fatalf("mixed snapshot: enabled=%v permission=%q", setting.Enabled, setting.Permission)
		}
	}
}

func TestConcurrentCallsAndOverridesAreSafe(t *testing.T) {
	ctx := context.Background()
	reg, _, policy, _ := wire(t, toolpolicy.Config{})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if worker%2 == 0 {
					// Reads race the writers; either outcome is valid.
					_ = execute(reg, "test.read")
					_ = execute(reg, "test.act")
				} else {
					if _, err := policy.Set(ctx, "test.read", toolpolicy.Update{Enabled: boolPtr(j%2 == 0)}); err != nil {
						t.Errorf("Set: %v", err)
						return
					}
					if _, err := policy.Set(ctx, "test.act", toolpolicy.Update{Permission: permissionPtr(tool.PermissionConfirm)}); err != nil {
						t.Errorf("Set permission: %v", err)
						return
					}
					if _, err := policy.Reset(ctx, "test.act"); err != nil {
						t.Errorf("Reset: %v", err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
}
