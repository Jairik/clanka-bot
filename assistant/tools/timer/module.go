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

// Register adds timer.set, timer.cancel and timer.list.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	var say func(ctx context.Context, text string) error
	if deps.Speaker != nil {
		say = deps.Speaker.SayText
	}
	return Register(r, NewManagerText(deps.Clock, say, deps.Logger, m.cfg.AlertText))
}
