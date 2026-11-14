package clock

import "github.com/Jairik/clanka-bot/assistant/internal/tool"

// Module is the clock Tool domain. It has no settings, so tools.clock is not
// a valid config section.
type Module struct{}

// NewModule builds the clock module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "clock" }

// Register adds time.now, date.today, time.in_city and date.countdown.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := Register(r, deps.Clock); err != nil {
		return err
	}
	if err := RegisterDate(r, deps.Clock); err != nil {
		return err
	}
	if err := RegisterCity(r, deps.Clock); err != nil {
		return err
	}
	return RegisterCountdown(r, deps.Clock, deps.Zone)
}
