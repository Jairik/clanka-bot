package tool

import "context"

// Path is the code path that made a Tool call.
type Path string

const (
	PathConfirmation  Path = "confirmation"
	PathControlPhrase Path = "control_phrase"
	// PathFocusIntent is a shared Control phrase delivered to whatever held
	// Focus: a ringing alert, the current reply, or music.
	PathFocusIntent Path = "focus_intent"
	PathFast        Path = "fast_path"
	PathAgent       Path = "agent_path"
	PathFallthrough Path = "fallthrough"
)

// Device is the originating device for a request.
type Device string

const (
	DevicePi        Device = "pi"
	DeviceSatellite Device = "satellite"
)

// Request is per-turn metadata carried on ctx: the request identity for
// logging and privacy decisions, and the wake model that started the turn. It
// is not model-visible text; Tools read what they need from it.
type Request struct {
	ID     string
	Path   Path
	Device Device
	// WakeWord names the wake model that started this turn. Empty for typed
	// requests. Later work (#81) uses it to act on which word fired.
	WakeWord string
}

type requestKey struct{}

// WithRequest attaches request metadata to ctx for a Tool handler.
func WithRequest(ctx context.Context, req Request) context.Context {
	return context.WithValue(ctx, requestKey{}, req)
}

// RequestFrom reads request metadata from ctx.
func RequestFrom(ctx context.Context) (Request, bool) {
	req, ok := ctx.Value(requestKey{}).(Request)
	return req, ok
}
