package tool

// DefaultFailureMessage is the only text shown for internal Tool errors.
const DefaultFailureMessage = "Something went wrong."

// ToolError is a user-facing failure. Say is spoken as-is.
type ToolError struct {
	Say string
}

func (e *ToolError) Error() string { return "tool: " + e.Say }
