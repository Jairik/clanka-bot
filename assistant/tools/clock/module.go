package clock

import "github.com/Jairik/clanka-bot/assistant/internal/tool"

// Module is the clock Tool domain. It has no settings, so tools.clock is not
// a valid config section.
type Module struct{}

// NewModule builds the clock module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "clock" }

// Register adds time.now and date.today.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := Register(r, deps.Clock); err != nil {
		return err
	}
	return RegisterDate(r, deps.Clock)
}
