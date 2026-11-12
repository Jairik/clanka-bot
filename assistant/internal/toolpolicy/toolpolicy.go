// Package toolpolicy turns a registry's code-declared Tool metadata into
// overridable settings. A stored override can disable a Tool or tighten its
// Permission (read -> act -> confirm); it can never loosen a Permission or
// change Local-only. The registry consults the policy on every call, so a
// change applies to the next request without a restart.
package toolpolicy

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// ErrUnknownTool reports a Tool name that is not in the registry.
var ErrUnknownTool = errors.New("toolpolicy: unknown tool")

// EnabledKey is a Tool's enabled setting key.
func EnabledKey(name string) string { return "tools." + name + ".enabled" }

// PermissionKey is a Tool's Permission setting key.
func PermissionKey(name string) string { return "tools." + name + ".permission" }

// Config is the config.yaml layer for Tool overrides.
type Config struct {
	// Disabled lists canonical Tool names turned off by config.yaml.
	Disabled []string
	// Permissions maps canonical Tool names to a tightened Permission.
	Permissions map[string]string
}

// State is one Tool's effective availability and the layer each value came
// from. LocalOnly is code-declared and never overridable.
type State struct {
	Name             string          `json:"name"`
	Enabled          bool            `json:"enabled"`
	Permission       tool.Permission `json:"permission"`
	Declared         tool.Permission `json:"declared_permission"`
	LocalOnly        bool            `json:"local_only"`
	EnabledSource    settings.Source `json:"enabled_source"`
	PermissionSource settings.Source `json:"permission_source"`
}

// Update is a partial change to one Tool. A nil field is left alone.
type Update struct {
	Enabled    *bool
	Permission *tool.Permission
}

// Policy is the live tool.Registry Policy implementation.
type Policy struct {
	settings *settings.Service
	declared map[string]tool.Info
	names    []string
}

// Definitions builds the enabled and permission settings for every registered
// Tool, including the config.yaml layer. It must run before New installs the
// policy, while registry.Info still returns the code declarations.
func Definitions(reg *tool.Registry, cfg Config) ([]settings.Definition, error) {
	if reg == nil {
		return nil, fmt.Errorf("toolpolicy: registry is required")
	}
	names := reg.Names()
	declared := make(map[string]tool.Info, len(names))
	for _, name := range names {
		info, ok := reg.Info(name)
		if !ok {
			return nil, fmt.Errorf("toolpolicy: %s disappeared from the registry", name)
		}
		declared[name] = info
	}

	disabled := make(map[string]bool, len(cfg.Disabled))
	for _, name := range cfg.Disabled {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("toolpolicy: tools.disabled names unknown Tool %q", name)
		}
		disabled[name] = true
	}
	configured := make(map[string]tool.Permission, len(cfg.Permissions))
	for name, level := range cfg.Permissions {
		info, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("toolpolicy: tools.permissions names unknown Tool %q", name)
		}
		permission := tool.Permission(level)
		if !permission.Valid() {
			return nil, fmt.Errorf("toolpolicy: tools.permissions.%s = %q, want read, act or confirm", name, level)
		}
		if !permission.AtLeast(info.Permission) {
			return nil, fmt.Errorf("toolpolicy: tools.permissions.%s = %q loosens the code-declared %q", name, level, info.Permission)
		}
		configured[name] = permission
	}

	defs := make([]settings.Definition, 0, 2*len(names))
	for _, name := range names {
		info := declared[name]
		enabledOptions := []settings.Option{
			settings.WithDescription("Whether " + name + " is available to requests."),
		}
		if disabled[name] {
			enabledOptions = append(enabledOptions, settings.WithConfig(false))
		}
		defs = append(defs, settings.Bool(EnabledKey(name), true, enabledOptions...))

		permissionOptions := []settings.Option{
			settings.WithDescription("Permission level for " + name + "; overrides may only tighten."),
			settings.WithValidator(permissionValidator(info.Permission)),
		}
		if level, ok := configured[name]; ok {
			permissionOptions = append(permissionOptions, settings.WithConfig(string(level)))
		}
		defs = append(defs, settings.String(PermissionKey(name), string(info.Permission), permissionOptions...))
	}
	return defs, nil
}

// permissionValidator rejects an invalid level or one that would loosen the
// Tool's code-declared Permission.
func permissionValidator(declared tool.Permission) func(any) error {
	return func(value any) error {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("want a Permission string, got %T", value)
		}
		permission := tool.Permission(text)
		if !permission.Valid() {
			return fmt.Errorf("want read, act or confirm, got %q", text)
		}
		if !permission.AtLeast(declared) {
			return fmt.Errorf("cannot loosen the code-declared %q", declared)
		}
		return nil
	}
}

// New verifies the Tool settings exist, records the code declarations and
// installs the policy on the registry.
func New(svc *settings.Service, reg *tool.Registry) (*Policy, error) {
	if svc == nil {
		return nil, fmt.Errorf("toolpolicy: settings service is required")
	}
	if reg == nil {
		return nil, fmt.Errorf("toolpolicy: registry is required")
	}
	names := reg.Names()
	sort.Strings(names)
	declared := make(map[string]tool.Info, len(names))
	for _, name := range names {
		info, ok := reg.Info(name)
		if !ok {
			return nil, fmt.Errorf("toolpolicy: %s disappeared from the registry", name)
		}
		if _, ok := svc.Get(EnabledKey(name)); !ok {
			return nil, fmt.Errorf("toolpolicy: %s has no enabled setting", name)
		}
		if _, ok := svc.Get(PermissionKey(name)); !ok {
			return nil, fmt.Errorf("toolpolicy: %s has no permission setting", name)
		}
		declared[name] = info
	}
	p := &Policy{settings: svc, declared: declared, names: names}
	reg.SetPolicy(p)
	return p, nil
}

// Effective implements tool.Policy for the registry's hot path. It fails
// closed: a missing or invalid Permission falls back to the code declaration,
// and the stored value can never loosen it.
func (p *Policy) Effective(name string, declared tool.Permission) tool.ToolSettings {
	enabled := true
	if value, ok := p.settings.Get(EnabledKey(name)); ok {
		if flag, ok := value.Value.(bool); ok {
			enabled = flag
		}
	}
	permission := declared
	if value, ok := p.settings.Get(PermissionKey(name)); ok {
		if text, ok := value.Value.(string); ok {
			candidate := tool.Permission(text)
			if candidate.Valid() && candidate.AtLeast(declared) {
				permission = candidate
			}
		}
	}
	return tool.ToolSettings{Enabled: enabled, Permission: permission}
}

// Get returns one Tool's state.
func (p *Policy) Get(name string) (State, bool) {
	info, ok := p.declared[name]
	if !ok {
		return State{}, false
	}
	enabledValue, _ := p.settings.Get(EnabledKey(name))
	permissionValue, _ := p.settings.Get(PermissionKey(name))
	return State{
		Name:             name,
		Enabled:          p.settings.Bool(EnabledKey(name)),
		Permission:       tool.Permission(p.settings.String(PermissionKey(name))),
		Declared:         info.Permission,
		LocalOnly:        info.LocalOnly,
		EnabledSource:    enabledValue.Source,
		PermissionSource: permissionValue.Source,
	}, true
}

// State returns one Tool's state, or the zero value for an unknown Tool.
func (p *Policy) State(name string) State {
	state, _ := p.Get(name)
	return state
}

// List returns every Tool state in name order.
func (p *Policy) List() []State {
	states := make([]State, 0, len(p.names))
	for _, name := range p.names {
		state, _ := p.Get(name)
		states = append(states, state)
	}
	return states
}

// Set validates and stores an override, then returns the new state. Both
// fields change in one settings batch; a rejected Permission leaves enabled
// untouched.
func (p *Policy) Set(ctx context.Context, name string, update Update) (State, error) {
	info, ok := p.declared[name]
	if !ok {
		return State{}, fmt.Errorf("%w %q", ErrUnknownTool, name)
	}
	values := make(map[string]any, 2)
	if update.Enabled != nil {
		values[EnabledKey(name)] = *update.Enabled
	}
	if update.Permission != nil {
		level := *update.Permission
		if !level.Valid() {
			return State{}, fmt.Errorf("toolpolicy: %s permission %q, want read, act or confirm", name, level)
		}
		if !level.AtLeast(info.Permission) {
			return State{}, fmt.Errorf("toolpolicy: %s permission %q would loosen the code-declared %q", name, level, info.Permission)
		}
		values[PermissionKey(name)] = string(level)
	}
	if len(values) == 0 {
		return State{}, fmt.Errorf("toolpolicy: update for %s is empty", name)
	}
	if err := p.settings.SetMany(ctx, values); err != nil {
		return State{}, err
	}
	return p.State(name), nil
}

// Reset removes both overrides so the config.yaml or code default applies.
func (p *Policy) Reset(ctx context.Context, name string) (State, error) {
	if _, ok := p.declared[name]; !ok {
		return State{}, fmt.Errorf("%w %q", ErrUnknownTool, name)
	}
	if err := p.settings.ResetMany(ctx, EnabledKey(name), PermissionKey(name)); err != nil {
		return State{}, err
	}
	return p.State(name), nil
}
