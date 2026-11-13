package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The Dashboard's Health page gives every check together one budget
// (dashboard.health_timeout, three seconds by default), while a cold DNS lookup
// through Tailscale MagicDNS plus a TLS handshake can need two seconds on its
// own (#68, PR #107). A live token check on every refresh therefore loses that
// race and reported "github: token check failed: context deadline exceeded"
// while the GitHub Tools answered in a few hundred milliseconds. The module
// remembers its last answer instead of asking again every minute.
const (
	// healthOKTTL is how long a successful check is reused. A rotated or
	// revoked token still shows up within it.
	healthOKTTL = 5 * time.Minute
	// healthFailTTL keeps a rejection visible without asking GitHub on every
	// single refresh.
	healthFailTTL = 30 * time.Second
	// healthBudget is the least shared budget a live check is worth starting
	// with: a warm lookup answers in a few hundred milliseconds, and a check
	// that overruns still falls back to the last known answer. Below it the
	// request would only report the snapshot's own deadline.
	healthBudget = time.Second
)

// memo is the last answer of one health check. The zero value is empty, so a
// module that was never checked runs a live check on first use.
type memo[T any] struct {
	mu    sync.Mutex
	set   bool
	at    time.Time
	value T
	fail  bool
}

// fresh returns the remembered answer while it is younger than its TTL.
func (m *memo[T]) fresh(now time.Time) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var zero T
	if !m.set || now.Sub(m.at) >= m.ttl() {
		return zero, false
	}
	return m.value, true
}

// last returns the remembered answer whatever its age, with the age itself.
func (m *memo[T]) last(now time.Time) (T, time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var zero T
	if !m.set {
		return zero, 0, false
	}
	age := now.Sub(m.at)
	if age < 0 {
		age = 0
	}
	return m.value, age, true
}

// record stores one answer and whether it was a failure.
func (m *memo[T]) record(now time.Time, value T, fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set, m.at, m.value, m.fail = true, now, value, fail
}

// ttl is the reuse window for the remembered answer.
func (m *memo[T]) ttl() time.Duration {
	if m.fail {
		return healthFailTTL
	}
	return healthOKTTL
}

// clock is the module's time source for health ages. Production leaves the
// field nil; tests inject one to exercise the TTL and the stale paths without
// waiting.
func (m *Module) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// shortOfBudget reports whether the shared snapshot deadline leaves less time
// than a live check needs.
func shortOfBudget(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < healthBudget
}

// spentDeadline reports whether err is the snapshot's own deadline rather than
// an answer from GitHub.
func spentDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// stale labels a reused answer with its age so it never reads as fresh. A
// stale success stays a success: the module health seam carries only an error,
// and claiming a failure we cannot see would be the opposite mistake.
func stale(err error, age time.Duration) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (last checked %s ago)", err, ago(age))
}

// ago renders an age for the Health row.
func ago(age time.Duration) string {
	if age < time.Second {
		return "under a second"
	}
	return age.Round(time.Second).String()
}
