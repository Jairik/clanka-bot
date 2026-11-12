package audiocontrol

import (
	"errors"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Config is the tools.audio section.
type Config struct {
	StepPercent int `yaml:"step_percent"`
}

// DefaultConfig is one volume step of 10 percent.
func DefaultConfig() Config { return Config{StepPercent: StepPercent} }

func (c Config) validate() error {
	if c.StepPercent < 1 || c.StepPercent > 100 {
		// A 0 or negative step would make "louder" silent or backwards.
		return errors.New("step_percent must be in [1, 100]")
	}
	return nil
}

// Module is the audio Tool domain.
type Module struct {
	cfg Config
}

// NewModule builds the audio module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "audio" }

// Configure decodes tools.audio over the code defaults.
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

// Register adds audio.volume_set and audio.stop.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	return RegisterWithStep(r, deps.Audio, m.cfg.StepPercent)
}
