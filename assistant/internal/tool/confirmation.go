package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Confirmation errors. ErrConfirmationRequired is returned by Execute for a
// confirm Tool, and ErrNoPending when a yes has nothing to run.
var (
	ErrConfirmationRequired = errors.New("tool: confirmation required")
	ErrNoPending            = errors.New("tool: no pending confirmation")
)

// PendingConfirmation is the single confirm Tool call waiting for a spoken
// yes. It lasts one turn and never queues across turns.
type PendingConfirmation struct {
	Tool    string
	Args    json.RawMessage
	Summary string
}

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
	if e.info.Permission != PermissionConfirm {
		return PendingConfirmation{}, fmt.Errorf("tool %q: permission %q needs no confirmation", name, e.info.Permission)
	}
	pending := PendingConfirmation{
		Tool:    e.info.Name,
		Args:    append(json.RawMessage(nil), args...),
		Summary: e.summary(args),
	}
	r.pendingMu.Lock()
	r.pending = &pending
	r.pendingMu.Unlock()
	return pending, nil
}

// Pending returns the waiting call, if any.
func (r *Registry) Pending() (PendingConfirmation, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if r.pending == nil {
		return PendingConfirmation{}, false
	}
	return *r.pending, true
}

// ExecutePending runs and consumes the stored call. The pending call is
// cleared before the handler runs, so it can run at most once.
func (r *Registry) ExecutePending(ctx context.Context) (any, PendingConfirmation, error) {
	r.pendingMu.Lock()
	pending := r.pending
	r.pending = nil
	r.pendingMu.Unlock()
	if pending == nil {
		return nil, PendingConfirmation{}, ErrNoPending
	}

	e, ok := r.lookup(pending.Tool)
	if !ok || e.info.Permission != PermissionConfirm {
		return nil, *pending, fmt.Errorf("tool %q: pending call no longer names a confirm Tool", pending.Tool)
	}
	result, err := r.run(ctx, e, pending.Args)
	return result, *pending, err
}

// DropPending discards the waiting call. Cancellation and Barge-in (#25) use
// it so a stale yes cannot run later.
func (r *Registry) DropPending() (PendingConfirmation, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if r.pending == nil {
		return PendingConfirmation{}, false
	}
	dropped := *r.pending
	r.pending = nil
	return dropped, true
}

// defaultSummary derives one spoken question from a Tool description.
func defaultSummary(name, description string) string {
	text := strings.TrimSpace(description)
	if text == "" {
		return fmt.Sprintf("Confirm %s?", name)
	}
	return strings.TrimSuffix(text, ".") + "?"
}
