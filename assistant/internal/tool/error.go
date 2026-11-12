package tool

import (
	"errors"
	"fmt"
)

// DefaultFailureMessage is the only text shown for internal Tool errors.
const DefaultFailureMessage = "Something went wrong."

// DisabledMessage is spoken when a stored override has turned a Tool off.
const DisabledMessage = "That tool is turned off."

// ErrToolDisabled marks a call refused by a stored disable override.
var ErrToolDisabled = errors.New("tool: disabled")

// DisabledError reports a Tool that a live override has turned off. The
// handler did not run.
type DisabledError struct {
	Tool string
}

func (e *DisabledError) Error() string {
	return fmt.Sprintf("tool %q: %v", e.Tool, ErrToolDisabled)
}

// Unwrap makes errors.Is(err, ErrToolDisabled) work.
func (e *DisabledError) Unwrap() error { return ErrToolDisabled }

// ToolError is a user-facing failure. Say is spoken as-is.
type ToolError struct {
	Say string
}

func (e *ToolError) Error() string { return "tool: " + e.Say }
