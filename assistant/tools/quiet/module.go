// Package quiet implements the quiet.set, quiet.off, quiet.dnd and quiet.awake
// Tools. They are act-only and LocalOnlyYes: the words only change the
// Assistant's own stored Quiet overrides, and the reply confirming the change
// stays on the Pi. The same settings keys are edited by the Dashboard.
package quiet

import (
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	SetName   = "quiet.set"
	OffName   = "quiet.off"
	DNDName   = "quiet.dnd"
	AwakeName = "quiet.awake"
)

// Module is the quiet Tool domain. The live window and volume are settings,
// not a tools.quiet block, so there is no module config to decode.
type Module struct{}

// NewModule builds the quiet module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "quiet" }

// Register adds every Quiet Tool. The handlers write through tool.Deps.Quiet,
// which main binds to the same settings service the Dashboard uses.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	service := deps.Quiet
	if err := tool.Add(r, NewSet(service)); err != nil {
		return err
	}
	if err := tool.Add(r, NewOff(service)); err != nil {
		return err
	}
	if err := tool.Add(r, NewDND(service)); err != nil {
		return err
	}
	return tool.Add(r, NewAwake(service))
}
