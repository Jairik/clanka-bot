// Package conversation implements the conversation-mode toggle Tools. They
// are act-only and LocalOnlyYes: the mode is an in-memory latch on the Pi and
// the spoken acknowledgement never leaves it. The longer budget and the raised
// Follow-up window are read from the same latch, not from a stored setting.
package conversation

import (
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	StartName = "conversation.start"
	EndName   = "conversation.end"
)

// Module is the conversation Tool domain. The mode itself is a shared seam,
// not a tools.conversation block, so there is no module config to decode.
type Module struct{}

// NewModule builds the conversation module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "conversation" }

// Register adds conversation.start and conversation.end. The handlers write
// through tool.Deps.Conversation, which main shares with the Agent path and
// the Follow-up window.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := tool.Add(r, NewStart(deps.Conversation)); err != nil {
		return err
	}
	return tool.Add(r, NewEnd(deps.Conversation))
}
