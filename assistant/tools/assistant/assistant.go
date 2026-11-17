// Package assistant implements the self-describing Tools: assistant.repeat
// speaks the previous completed reply again, assistant.help is generated from
// the live Tool registry, and assistant.forget clears the shared conversation
// history. All three are Local-only: a repeated reply may have come from a
// Local-only Tool, a help list names Tools the Agent path is not allowed to
// see, and a forget confirmation must work with no provider reachable, so no
// result may leave the Pi.
package assistant

import (
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	RepeatName        = "assistant.repeat"
	HelpName          = "assistant.help"
	ForgetName        = "assistant.forget"
	AnnouncementsName = "assistant.announcements"
)

// Module is the assistant Tool domain. It has no settings; the previous reply
// and the registry are shared seams, not configuration.
type Module struct{}

// NewModule builds the assistant module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "assistant" }

// Register adds assistant.repeat, assistant.help, assistant.forget and
// assistant.announcements. The help handler closes over the registry and reads
// effective metadata on every call, so a stored disable or a newly registered
// Tool applies without re-registration. The forget handler closes over the
// shared history seam that main binds before this module registers.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	// The repeat's speed step falls back to the live voice.rate when the last
	// reply carried no per-utterance override. Voices is nil in conformance and
	// some tests; NewRepeat then uses 1.0 as the neutral base.
	var rate func() float64
	if deps.Voices != nil {
		rate = deps.Voices.Rate
	}
	if err := tool.Add(r, NewRepeat(deps.LastReply, rate)); err != nil {
		return err
	}
	if err := tool.Add(r, NewForget(deps.History)); err != nil {
		return err
	}
	if err := tool.Add(r, NewAnnouncements(deps.Announcements)); err != nil {
		return err
	}
	return tool.Add(r, NewHelp(r))
}
