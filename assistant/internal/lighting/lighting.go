// Package lighting holds the seam between the two lighting transports.
//
// Room controllers are Bluetooth GATT writes (internal/lights, tools/lights);
// the PC / desk bar is a Monster cloud device (internal/monster,
// tools/monster). A general command ("turn the lights off") addresses both, so
// the Bluetooth module owns the group and reaches the cloud through the
// Transport below. The seam is small on purpose: it carries no device model
// and no transport detail, only what a group command needs.
package lighting

import "context"

// Transport is one side of a general lighting command, as the group owner sees
// it. The Monster module implements it; a test can supply a fake.
type Transport interface {
	// Targets reports whether the words name this transport's own light
	// ("pc light", "monitor light", "light bar"). The group owner uses it so a
	// command for the PC light is not filled from a saved Bluetooth name.
	Targets(text string) bool
	// ApplyEvery applies op to every light the transport owns. The group owner
	// runs it concurrently with its own writes.
	ApplyEvery(ctx context.Context, op, value string) Result
}

// Result reports one group command on a transport. Total 0 with a nil Err
// means the transport has no lights at all — no account, or an empty one — and
// a general command skips it without complaint. A non-nil Err with Total 0
// means the transport owns lights but could not be reached, which the reply
// must say out loud.
type Result struct {
	Applied int
	Total   int
	Err     error
}

// Addressed reports whether the transport had anything to do: it owns lights,
// or it failed while trying to find them.
func (r Result) Addressed() bool { return r.Total > 0 || r.Err != nil }
