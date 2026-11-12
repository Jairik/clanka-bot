package timer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
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
	// manager is created during Register; main attaches the #52 Ring engine
	// there and the Start hook recovers persisted Timers after the live
	// settings, Cues, Zone and Ring seams are bound.
	manager *Manager
	// deps is kept so Start can attach the store and Announcement queue.
	deps tool.Deps
}

// NewModule builds the timer module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Manager exposes the running Manager so main can attach the Ring engine once
// the live settings service is available. It is nil before Register.
func (m *Module) Manager() *Manager { return m.manager }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "timer" }

// Intents declares the shared Focus intents a ringing Timer handles. "snooze"
// is included because, on a Timer, it acts as stop; #53 owns Alarm snooze.
func (*Module) Intents() []focus.Intent { return alertIntents() }

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

// Register adds timer.set, timer.cancel, timer.remaining and timer.list. The
// persisted Timers are recovered by Start, not here, so the first recovered
// Ring already sees the effective settings, Cues, Zone and Ring seams.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	var say func(ctx context.Context, text string) error
	if deps.Speaker != nil {
		say = deps.Speaker.SayText
	}
	manager := NewManagerText(deps.Clock, say, deps.Logger, m.cfg.AlertText)
	if deps.Focus != nil {
		manager = manager.WithFocus(deps.Focus)
	}
	// Attach the Ring engine before Start's recovery so an overdue Timer that
	// fires immediately at startup already Rings through the shared engine.
	if deps.Ring != nil {
		manager.SetRingEngine(deps.Ring)
	}
	// Install the missed Notification hook before Start's recovery: recovery
	// calls it for an open row left by a crash and for an old overdue Timer, so
	// those configured Notifications are not lost at startup.
	if deps.Notify != nil {
		notifier := deps.Notify
		manager.SetMissedHook(func(alert MissedAlert) {
			notifier.NotifyMissed(context.Background(), notify.MissedAlert{
				Kind:     alert.Kind,
				Label:    alert.Label,
				Reason:   alert.Reason,
				Duration: time.Duration(alert.DurationSeconds) * time.Second,
				FiresAt:  alert.FiresAt,
				FiredAt:  alert.FiredAt,
				EndedAt:  alert.EndedAt,
			})
		})
	}
	m.deps = deps
	m.manager = manager
	return Register(r, manager)
}

// Start recovers the Timers left by the previous process. It runs after main
// has bound the live settings service, Cues, Zone and Ring engine, so the
// first recovered Ring obeys stored settings rather than the code defaults.
func (m *Module) Start(context.Context) error {
	if m.manager == nil {
		return nil
	}
	return m.manager.AttachStore(m.deps.Store, m.deps.Announcements)
}
