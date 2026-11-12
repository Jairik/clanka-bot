// Package tools holds the one list of Tool modules and wires them into the
// registry. Adding a Tool domain is one line in NewSet.
package tools

import (
	"context"
	"fmt"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
	"github.com/Jairik/clanka-bot/assistant/tools/voice"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
	"gopkg.in/yaml.v3"
)

// Set is the wired Tool modules, in registration order.
type Set struct {
	modules []tool.Module
}

// NewSet returns the module list. This is the only place modules are wired.
func NewSet() *Set {
	return &Set{modules: []tool.Module{
		clock.NewModule(),
		weather.NewModule(),
		audiocontrol.NewModule(),
		timer.NewModule(),
		voice.NewModule(),
	}}
}

// Modules returns the wired modules in registration order.
func (s *Set) Modules() []tool.Module {
	return append([]tool.Module(nil), s.modules...)
}

// Module returns the one wired module with name, or nil. main uses it to
// attach a live seam that only exists after this package is constructed, such
// as the Timer Ring engine.
func (s *Set) Module(name string) tool.Module {
	for _, module := range s.modules {
		if module.Name() == name {
			return module
		}
	}
	return nil
}

// SecretNames reports every secret declared by the modules, in module order,
// with duplicates collapsed. main adds the Agent provider's key name and hands
// the list to the secrets service as the write-only allowlist.
func (s *Set) SecretNames() []string {
	return tool.SecretNames(s.modules...)
}

// FocusIntents reports every shared Focus intent a module declares, as
// "module:intent", in module order. main logs it at startup so the wiring is
// visible; the conformance suite validates each declaration.
func (s *Set) FocusIntents() []string {
	var names []string
	for _, module := range s.modules {
		declarer, ok := module.(tool.IntentDeclarer)
		if !ok {
			continue
		}
		for _, intent := range declarer.Intents() {
			names = append(names, module.Name()+":"+string(intent))
		}
	}
	return names
}

// RegisterAll configures each module from its tools.<name> section and adds
// its Tools. A nil sections map uses the modules' code defaults.
func (s *Set) RegisterAll(r *tool.Registry, deps tool.Deps, sections map[string]yaml.Node) error {
	known := make(map[string]bool, len(s.modules))
	for _, module := range s.modules {
		known[module.Name()] = true
	}
	for name := range sections {
		if !known[name] {
			return fmt.Errorf("tools.%s: no such Tool module", name)
		}
	}

	for _, module := range s.modules {
		section, hasSection := sections[module.Name()]
		if configurable, ok := module.(tool.Configurable); ok {
			var node *yaml.Node
			if hasSection {
				node = &section
			}
			if err := configurable.Configure(node); err != nil {
				return fmt.Errorf("tools.%s: %w", module.Name(), err)
			}
		} else if hasSection {
			return fmt.Errorf("tools.%s: module has no settings", module.Name())
		}
		if err := module.Register(r, deps); err != nil {
			return fmt.Errorf("tools.%s: %w", module.Name(), err)
		}
	}
	return nil
}

// Health runs every module's optional health check, in module order.
func (s *Set) Health(ctx context.Context) []Health {
	results := make([]Health, 0, len(s.modules))
	for _, module := range s.modules {
		checker, ok := module.(tool.HealthChecker)
		if !ok {
			continue
		}
		results = append(results, Health{Module: module.Name(), Err: checker.Health(ctx)})
	}
	return results
}

// Health is one module's health-check result.
type Health struct {
	Module string
	Err    error
}
