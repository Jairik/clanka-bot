package alarm

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the alarm Tool domain. It has no tools.alarm config section; the
// Alarm Ring and snooze behaviour are live settings under ring.alarm.*.
type Module struct {
	// manager is created during Register; Start recovers persisted Alarms
	// after main has bound the live settings, Cues, Zone and Ring seams.
	manager *Manager
	deps    tool.Deps
}

// NewModule builds the alarm module.
func NewModule() *Module { return &Module{} }

// Name is the module name and the optional config section suffix.
func (*Module) Name() string { return "alarm" }

// Intents declares the shared Focus intents a ringing Alarm handles. Unlike a
// Timer, snooze is real: the engine ends the Ring and the scheduler re-rings.
func (*Module) Intents() []focus.Intent {
	return []focus.Intent{focus.Stop, focus.Cancel, focus.Snooze}
}

// Register adds alarm.set, alarm.list and alarm.cancel. Persisted Alarms are
// recovered by Start, not here, so the first recovered Ring already sees the
// effective settings, Cues, Zone and Ring seams.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	var say func(context.Context, string) error
	if deps.Speaker != nil {
		say = deps.Speaker.SayText
	}
	manager := NewManager(Options{
		Now:           deps.Clock,
		Say:           say,
		Logger:        deps.Logger,
		Zone:          deps.Zone,
		Settings:      deps.Settings,
		Announcements: deps.Announcements,
		Cues:          deps.Cues,
		Names:         deps.Names,
	})
	// Attach the Ring engine before Start's recovery so an overdue Alarm that
	// fires immediately at startup already Rings through the shared engine.
	if deps.Ring != nil {
		manager.SetRingEngine(deps.Ring)
	}
	// Install the missed Notification hook before Start's recovery: recovery
	// calls it for an open row left by a crash and for an old overdue Alarm, so
	// those configured Notifications are not lost at startup.
	if deps.Notify != nil {
		notifier := deps.Notify
		manager.SetMissedHook(func(alert MissedAlert) {
			notifier.NotifyMissed(context.Background(), notify.MissedAlert{
				Kind:    alert.Kind,
				Label:   alert.Label,
				Reason:  alert.Reason,
				FiresAt: alert.FiresAt,
				FiredAt: alert.FiredAt,
				EndedAt: alert.EndedAt,
			})
		})
	}
	m.deps = deps
	m.manager = manager
	return Register(r, manager)
}

// Start recovers the Alarms left by the previous process. It runs after main
// has bound the live settings service, Cues, Zone and Ring engine, so the
// first recovered Ring obeys stored settings rather than the code defaults.
func (m *Module) Start(context.Context) error {
	if m.manager == nil {
		return nil
	}
	return m.manager.AttachStore(m.deps.Store, m.deps.Announcements)
}

// Manager exposes the running Manager so main can install the missed hook and
// close it at shutdown. It is nil before Register.
func (m *Module) Manager() *Manager { return m.manager }
