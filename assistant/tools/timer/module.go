package timer

import (
	"context"
	"errors"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Config is the tools.timer section.
type Config struct {
	AlertText string `yaml:"alert_text"`
}

// DefaultConfig is the shipped alert phrase.
func DefaultConfig() Config { return Config{AlertText: AlertText} }

func (c Config) validate() error {
	if strings.TrimSpace(c.AlertText) == "" {
		return errors.New("alert_text must not be empty")
	}
	return nil
}

// Module is the timer Tool domain.
type Module struct {
	cfg Config
}

// NewModule builds the timer module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "timer" }

// Configure decodes tools.timer over the code defaults.
func (m *Module) Configure(section *yaml.Node) error {
	cfg := DefaultConfig()
	if section != nil {
		if err := section.Decode(&cfg); err != nil {
			return err
		}
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// Register adds timer.set, timer.cancel, timer.remaining and timer.list. When
// the shared store is present the Manager recovers the Timers left by the
// previous process and applies the downtime rule before any request arrives.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	var say func(ctx context.Context, text string) error
	if deps.Speaker != nil {
		say = deps.Speaker.SayText
	}
	manager := NewManagerText(deps.Clock, say, deps.Logger, m.cfg.AlertText)
	if err := manager.AttachStore(deps.Store, deps.Announcements); err != nil {
		return err
	}
	return Register(r, manager)
}
