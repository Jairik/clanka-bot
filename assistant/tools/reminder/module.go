package reminder

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the reminder Tool domain. The Reminder has no tools.reminder
// settings: the Cue and quiet.volume are live settings shared with the sound
// and Quiet-hours services.
type Module struct {
	// manager is created during Register; Start recovers persisted Reminders
	// after main has bound the live settings, Cues, Zone and Quiet seams.
	manager *Manager
	deps    tool.Deps
}

// NewModule builds the reminder module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "reminder" }

// Manager exposes the live Manager for main and the tests. It is nil before
// Register.
func (m *Module) Manager() *Manager { return m.manager }

// Register adds every Reminder Tool. Persisted Reminders are recovered by
// Start, not here, so the first recovered alert already sees the effective
// Cue, Quiet volume and timezone. The module reads no settings at Register.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	manager := NewManager(Options{
		Now:     deps.Clock,
		Logger:  deps.Logger,
		Zone:    deps.Zone,
		Speaker: deps.Speaker,
		Queue:   deps.Announcements,
		Cues:    deps.Cues,
		Quiet:   deps.Quiet,
	})
	m.deps = deps
	m.manager = manager
	return Register(r, manager)
}

// Start recovers the Reminders left by the previous process. It runs through
// the module-list Startable hook after main has bound the live settings, Cues,
// Zone and Quiet hours, so an overdue Reminder recovered at startup already
// uses the stored configuration instead of the code defaults.
func (m *Module) Start(context.Context) error {
	if m.manager == nil {
		return nil
	}
	return m.manager.AttachStore(m.deps.Store)
}

// Close stops the live Reminder manager. It is the optional Closer hook main
// calls at shutdown; a module that never registered is inert.
func (m *Module) Close() {
	if m.manager != nil {
		m.manager.Close()
	}
}
