package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Confirmation errors. ErrConfirmationRequired is returned by Execute for a
// confirm Tool, and ErrNoPending when a yes has nothing to run.
var (
	ErrConfirmationRequired = errors.New("tool: confirmation required")
	ErrNoPending            = errors.New("tool: no pending confirmation")
)

// PendingTTL bounds how long an unanswered Confirmation waits for its yes. It
// is long enough to cover the question's turn plus the user's thinking time,
// and short enough that walking away stops holding Announcements.
const PendingTTL = 60 * time.Second

// PendingConfirmation is the single confirm Tool call waiting for a spoken
// yes. It lasts one turn and never queues across turns.
type PendingConfirmation struct {
	ID      uint64
	Tool    string
	Args    json.RawMessage
	Summary string
}

var confirmationID atomic.Uint64

// ConfirmationRequiredError reports a confirm Tool that needs a Confirmation.
// The handler did not run and no pending call was stored.
type ConfirmationRequiredError struct {
	Pending PendingConfirmation
}

func (e *ConfirmationRequiredError) Error() string {
	return fmt.Sprintf("tool %q: %v", e.Pending.Tool, ErrConfirmationRequired)
}

// Unwrap makes errors.Is(err, ErrConfirmationRequired) work.
func (e *ConfirmationRequiredError) Unwrap() error { return ErrConfirmationRequired }

// Propose stores at most one pending confirm call and returns its one-line
// summary. It never runs the Tool. A newer proposal replaces the older one.
func (r *Registry) Propose(name string, args json.RawMessage) (PendingConfirmation, error) {
	e, ok := r.lookup(name)
	if !ok {
		return PendingConfirmation{}, fmt.Errorf("tool: unknown tool %q", name)
	}
	info := r.effective(e)
	if !info.Enabled {
		return PendingConfirmation{}, &DisabledError{Tool: name}
	}
	if info.Permission != PermissionConfirm {
		return PendingConfirmation{}, fmt.Errorf("tool %q: permission %q needs no confirmation", name, info.Permission)
	}
	// Arm nothing when the model's arguments do not satisfy the Tool's schema:
	// a malformed confirm call must not become a runnable pending call.
	if err := validateArgs(e.schema, args); err != nil {
		return PendingConfirmation{}, fmt.Errorf("tool %q: %w", name, err)
	}
	pending := PendingConfirmation{
		ID:      confirmationID.Add(1),
		Tool:    e.info.Name,
		Args:    append(json.RawMessage(nil), args...),
		Summary: e.summary(args),
	}
	r.pendingMu.Lock()
	r.pending = &pending
	r.pendingAt = r.nowTime()
	r.pendingMu.Unlock()
	return pending.clone(), nil
}

// Pending returns the waiting call, if any. A call older than PendingTTL is
// absent and is cleared here, so an abandoned question stops blocking
// Announcements without another request.
func (r *Registry) Pending() (PendingConfirmation, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if r.pending == nil || r.expireLocked() {
		return PendingConfirmation{}, false
	}
	return r.pending.clone(), true
}

// Inspecting a proposal cannot change the argument bytes that a yes will run.
func (pending PendingConfirmation) clone() PendingConfirmation {
	pending.Args = append(json.RawMessage(nil), pending.Args...)
	return pending
}

// ExecutePending runs and consumes the stored call. The pending call is
// cleared before the handler runs, so it can run at most once. An expired call
// is absent: a late yes cannot run the old outward-facing call.
func (r *Registry) ExecutePending(ctx context.Context) (any, PendingConfirmation, error) {
	return r.executePending(ctx, 0)
}

// ExecutePendingFor consumes only the matching proposal.
func (r *Registry) ExecutePendingFor(ctx context.Context, id uint64) (any, PendingConfirmation, error) {
	if id == 0 {
		return nil, PendingConfirmation{}, ErrNoPending
	}
	return r.executePending(ctx, id)
}

// A zero ID is reserved for unconditional consumers of the legacy API.
func (r *Registry) executePending(ctx context.Context, id uint64) (any, PendingConfirmation, error) {
	r.pendingMu.Lock()
	r.expireLocked()
	pending := r.pending
	if pending != nil && id != 0 && pending.ID != id {
		pending = nil
	}
	if pending != nil {
		r.pending = nil
		r.pendingAt = time.Time{}
	}
	r.pendingMu.Unlock()
	if pending == nil {
		return nil, PendingConfirmation{}, ErrNoPending
	}

	e, ok := r.lookup(pending.Tool)
	if !ok {
		return nil, *pending, fmt.Errorf("tool %q: pending call no longer names a confirm Tool", pending.Tool)
	}
	info := r.effective(e)
	if !info.Enabled {
		return nil, *pending, &DisabledError{Tool: pending.Tool}
	}
	if info.Permission != PermissionConfirm {
		return nil, *pending, fmt.Errorf("tool %q: pending call no longer names a confirm Tool", pending.Tool)
	}
	result, err := r.run(ctx, e, pending.Args)
	return result, *pending, err
}

// DropPendingFor discards only the observed proposal.
func (r *Registry) DropPendingFor(id uint64) bool {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if r.pending == nil || r.expireLocked() || r.pending.ID != id {
		return false
	}
	r.pending = nil
	r.pendingAt = time.Time{}
	return true
}

// DropPending discards the waiting call. Cancellation and Barge-in (#25) use
// it so a stale yes cannot run later. An expired call is already absent.
func (r *Registry) DropPending() (PendingConfirmation, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if r.pending == nil || r.expireLocked() {
		return PendingConfirmation{}, false
	}
	dropped := *r.pending
	r.pending = nil
	r.pendingAt = time.Time{}
	return dropped, true
}

// expireLocked clears the stored call once it is older than PendingTTL. The
// caller holds r.pendingMu. It reports whether a call was expired.
func (r *Registry) expireLocked() bool {
	if r.pending == nil || r.nowTime().Sub(r.pendingAt) < PendingTTL {
		return false
	}
	r.pending = nil
	r.pendingAt = time.Time{}
	return true
}

// nowTime reads the injected clock, defaulting to time.Now for a zero Registry.
func (r *Registry) nowTime() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// defaultSummary derives one spoken question from a Tool description.
func defaultSummary(name, description string) string {
	text := strings.TrimSpace(description)
	if text == "" {
		return fmt.Sprintf("Confirm %s?", name)
	}
	return strings.TrimSuffix(text, ".") + "?"
}
