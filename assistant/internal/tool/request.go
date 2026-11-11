package tool

import "context"

// Path is the code path that made a Tool call.
type Path string

const (
	PathControlPhrase Path = "control_phrase"
	PathFast          Path = "fast_path"
	PathAgent         Path = "agent_path"
	PathFallthrough   Path = "fallthrough"
)

// Device is the originating device for a request.
type Device string

const (
	DevicePi        Device = "pi"
	DeviceSatellite Device = "satellite"
)

// Request identifies a Tool call for logging and privacy decisions.
type Request struct {
	ID     string
	Path   Path
	Device Device
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
