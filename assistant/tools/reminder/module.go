package reminder

import (
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the reminder Tool domain. The Reminder has no tools.reminder
// settings: the Cue and quiet.volume are live settings shared with the sound
// and Quiet-hours services.
type Module struct {
	// manager is created during Register and stopped through the optional
	// tool.Closer hook at shutdown.
	manager *Manager
}

// NewModule builds the reminder module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "reminder" }

// Manager exposes the live Manager for main and the tests. It is nil before
// Register.
func (m *Module) Manager() *Manager { return m.manager }

// Register adds every Reminder Tool. Recovery happens here, before serving, so
// an overdue Reminder recovered at startup is already scheduled. The module
// reads no settings at registration: the live timezone, Cue, Quiet volume and
// speaker are all read when a Reminder is set or fires, so #53 can move module
// startup without changing this package.
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
	if err := manager.AttachStore(deps.Store); err != nil {
		return err
	}
	m.manager = manager
	return Register(r, manager)
}

// Close stops the live Reminder manager. It is the optional lifecycle hook
// main calls at shutdown; a module that never registered is inert.
func (m *Module) Close() {
	if m.manager != nil {
		m.manager.Close()
	}
}
