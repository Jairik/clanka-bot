// Package assistant implements the self-describing Tools: assistant.repeat
// speaks the previous completed reply again, and assistant.help is generated
// from the live Tool registry. Both are Local-only: a repeated reply may have
// come from a Local-only Tool, and a help list names Tools the Agent path is
// not allowed to see, so neither result may leave the Pi.
package assistant

import (
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	RepeatName = "assistant.repeat"
	HelpName   = "assistant.help"
)

// Module is the assistant Tool domain. It has no settings; the previous reply
// and the registry are shared seams, not configuration.
type Module struct{}

// NewModule builds the assistant module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "assistant" }

// Register adds assistant.repeat and assistant.help. The help handler closes
// over the registry and reads effective metadata on every call, so a stored
// disable or a newly registered Tool applies without re-registration.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := tool.Add(r, NewRepeat(deps.LastReply)); err != nil {
		return err
	}
	return tool.Add(r, NewHelp(r))
}
