package alarm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// errFireStore is the injected store error of a failed fire transaction.
var errFireStore = errors.New("injected store error: database is locked")

// fireClock is the scheduler's injected wall time for these tests.
type fireClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now reads the current injected instant.
func (c *fireClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the injected instant forward.
func (c *fireClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// fireSay records each spoken fallback line, which is what "the Alarm rang"
// means when no Ring engine is attached.
type fireSay struct {
	mu    sync.Mutex
	lines []string
	said  chan struct{}
}

// newFireSay builds a recorder with room for the few lines a test expects.
func newFireSay() *fireSay {
	return &fireSay{said: make(chan struct{}, 8)}
}

// SayText records one spoken line.
func (r *fireSay) SayText(_ context.Context, text string) error {
	r.mu.Lock()
	r.lines = append(r.lines, text)
	r.mu.Unlock()
	r.said <- struct{}{}
	return nil
}

// count returns how many lines were spoken so far.
func (r *fireSay) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// openFireStore opens the real database with the real migrations.
func openFireStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newFireManager builds a durable Manager with these test seams and a clock
// half a second before the Alarm, so the callback delay stays tiny.
func newFireManager(t *testing.T, st *store.Store, rec *fireSay) (*Manager, *fireClock) {
	t.Helper()
	clock := &fireClock{now: time.Date(2026, 11, 12, 6, 59, 59, 500_000_000, time.UTC)}
	manager := NewManager(Options{
		Now:    clock.Now,
		Say:    rec.SayText,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	return manager, clock
}

// setFireTx replaces the fire transaction under the Manager lock, so the
// callback goroutine observes it without a data race.
func setFireTx(manager *Manager, fn func(ctx context.Context, id string, snoozed bool) (fireState, bool, error)) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.fireTx = fn
}

// setAdvanceFire replaces the best-effort pre-Ring row advance, so a test can
// fail one write of the degraded path without failing the late history write.
func setAdvanceFire(manager *Manager, fn func(ctx context.Context, alarm Alarm, next *time.Time) error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.advanceFire = fn
}

// liveEntries reports how many callbacks are armed and the occurrence the
// scheduler will actually fire.
func liveEntries(manager *Manager) (int, time.Time) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	var occurrence time.Time
	for _, e := range manager.entries {
		occurrence = e.occurrence
	}
	return len(manager.entries), occurrence
}

// waitLiveEntry waits for exactly one armed callback at want. afterFire runs
// just after the history row appears, so asserting the instant it does races
// the reschedule.
func waitLiveEntry(t *testing.T, manager *Manager, want time.Time) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		count, occurrence := liveEntries(manager)
		if count == 1 && occurrence.Equal(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live entries = %d at %s, want 1 at %s", count, occurrence, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitSaid waits for one spoken fallback line.
func waitSaid(t *testing.T, rec *fireSay) {
	t.Helper()
	select {
	case <-rec.said:
	case <-time.After(3 * time.Second):
		t.Fatal("the Alarm never rang")
	}
}

// waitFireHistory waits for want terminal Alarm events and returns them.
func waitFireHistory(t *testing.T, manager *Manager, want int) []alertstore.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := manager.History(20)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		terminal := 0
		for _, event := range events {
			if event.Kind == "alarm" && !event.State.Open() {
				terminal++
			}
		}
		if terminal >= want {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("alarm history has %d terminal events, want %d: %+v", terminal, want, events)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitFireRows waits for the persisted Alarm count, because deleteDoneRow runs
// just after the history row appears.
func waitFireRows(t *testing.T, st *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&count); err != nil {
			t.Fatalf("count alarms: %v", err)
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("alarm rows = %d, want %d", count, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestFireRingsAfterOneStoreError covers the transient case: the first fire
// transaction fails, the one retry succeeds, and the Alarm rings with its
// history row for that occurrence (#173).
func TestFireRingsAfterOneStoreError(t *testing.T) {
	st := openFireStore(t)
	rec := newFireSay()
	manager, _ := newFireManager(t, st, rec)

	created, err := manager.Set(context.Background(), SetIn{Hour: 7, Label: "wake"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	var calls atomic.Int32
	setFireTx(manager, func(_ context.Context, id string, snoozed bool) (fireState, bool, error) {
		if calls.Add(1) == 1 {
			return fireState{}, false, errFireStore
		}
		return manager.beginFire(context.Background(), id, snoozed)
	})

	waitSaid(t, rec)
	if got := rec.count(); got != 1 {
		t.Fatalf("spoken lines = %d, want 1", got)
	}
	events := waitFireHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "spoken" {
		t.Fatalf("event = %+v, want stopped/spoken", events[0])
	}
	if !events[0].FiresAt.Equal(created.NextFiresAt) {
		t.Fatalf("event fires_at = %s, want the fired occurrence %s", events[0].FiresAt, created.NextFiresAt)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("fire transactions = %d, want 2 (one retry)", got)
	}
	waitFireRows(t, st, 0)
}

// TestFireRingsWhenTheStoreStaysUnavailable covers the second failure: the
// Alarm still rings from the captured entry, its outcome is recorded once a
// write works, and a repeating Alarm continues without ringing the same
// occurrence twice in this process (#173).
func TestFireRingsWhenTheStoreStaysUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule string
		repeat   bool
	}{
		{name: "one-off", schedule: ""},
		{name: "daily", schedule: "daily", repeat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFireStore(t)
			rec := newFireSay()
			manager, clock := newFireManager(t, st, rec)

			created, err := manager.Set(context.Background(), SetIn{Hour: 7, Schedule: tc.schedule, Label: "wake"})
			if err != nil {
				t.Fatalf("Set: %v", err)
			}
			var calls atomic.Int32
			setFireTx(manager, func(context.Context, string, bool) (fireState, bool, error) {
				calls.Add(1)
				return fireState{}, false, errFireStore
			})

			waitSaid(t, rec)
			events := waitFireHistory(t, manager, 1)
			if events[0].State != alertstore.StateStopped || events[0].EndedBy != "spoken" {
				t.Fatalf("event = %+v, want stopped/spoken", events[0])
			}
			if !events[0].FiresAt.Equal(created.NextFiresAt) {
				t.Fatalf("event fires_at = %s, want the fired occurrence %s",
					events[0].FiresAt, created.NextFiresAt)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("fire transactions = %d, want 2 attempts", got)
			}

			if tc.repeat {
				// The row was advanced before the Ring, so a restart does not
				// repeat this occurrence and the live schedule holds tomorrow.
				var nextMS int64
				if err := st.DB().QueryRow(`SELECT next_fires_at FROM alarms`).Scan(&nextMS); err != nil {
					t.Fatalf("read row: %v", err)
				}
				wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
				if nextMS != wantNext.UnixMilli() {
					t.Fatalf("row next = %s, want %s", time.UnixMilli(nextMS), wantNext)
				}
				list := manager.List()
				if len(list) != 1 || !list[0].NextFiresAt.Equal(wantNext) {
					t.Fatalf("List = %+v, want one Alarm next at %s", list, wantNext)
				}
			} else {
				// A finished one-off row is removed, so nothing can ring it again.
				waitFireRows(t, st, 0)
				if list := manager.List(); len(list) != 0 {
					t.Fatalf("List = %+v, want no alarms", list)
				}
			}

			// The same occurrence must not ring a second time in this process.
			clock.Set(clock.Now().Add(2 * time.Second))
			time.Sleep(300 * time.Millisecond)
			if got := rec.count(); got != 1 {
				t.Fatalf("spoken lines = %d, want 1 for the one occurrence", got)
			}
			waitFireHistory(t, manager, 1)
		})
	}
}

// TestDegradedSnoozeReringKeepsTheRepeat covers a repeating Alarm whose snooze
// re-ring meets a store that stays down. The row and List() already advertise
// tomorrow, so the in-process schedule must hold it too; otherwise the Alarm
// disappears from a running process while the Dashboard still shows it (#173).
func TestDegradedSnoozeReringKeepsTheRepeat(t *testing.T) {
	st := openFireStore(t)
	rec := newFireSay()
	manager, clock := newFireManager(t, st, rec)

	created, err := manager.Set(context.Background(), SetIn{Hour: 7, Schedule: "daily", Label: "wake"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)

	// The first Ring succeeds normally, so the row is already at tomorrow when
	// the snooze is taken.
	waitSaid(t, rec)
	waitFireHistory(t, manager, 1)
	waitLiveEntry(t, manager, wantNext)

	// Take the snooze the first Ring would have taken: the row's marker plus a
	// live callback at the re-ring target, 150 ms out in real time.
	clock.Set(created.Alarm.NextFiresAt)
	target := clock.Now().Add(150 * time.Millisecond)
	if _, err := st.DB().Exec(`UPDATE alarms SET snoozed_until = ? WHERE id = ?`,
		target.UnixMilli(), created.Alarm.ID); err != nil {
		t.Fatalf("seed snooze: %v", err)
	}
	manager.schedule(created.Alarm, target, true)

	// The snooze re-ring meets a store that never answers.
	setFireTx(manager, func(context.Context, string, bool) (fireState, bool, error) {
		return fireState{}, false, errFireStore
	})
	waitSaid(t, rec)

	// Exactly one live occurrence must remain, at the same next occurrence the
	// row and List() advertise.
	waitLiveEntry(t, manager, wantNext)
	list := manager.List()
	if len(list) != 1 || !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("List = %+v, want one Alarm next at %s", list, wantNext)
	}
	var nextMS int64
	var snoozeMS *int64
	if err := st.DB().QueryRow(`SELECT next_fires_at, snoozed_until FROM alarms`).Scan(&nextMS, &snoozeMS); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if nextMS != wantNext.UnixMilli() || snoozeMS != nil {
		t.Fatalf("row next = %s snooze = %v, want %s and NULL",
			time.UnixMilli(nextMS), snoozeMS, wantNext)
	}
}

// TestLateRecordedDegradedFireIsNotMissedOnRestart is the hard interleaving:
// the pre-Ring advance fails but the late history write lands. Recording the
// fire must therefore advance the row in the same transaction, or the next
// start reads the stopped row's past occurrence as a downtime miss and pushes
// a false "You missed an alarm" Notification (#173).
func TestLateRecordedDegradedFireIsNotMissedOnRestart(t *testing.T) {
	st := openFireStore(t)
	rec := newFireSay()
	clock := &fireClock{now: time.Date(2026, 11, 12, 6, 59, 59, 500_000_000, time.UTC)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var mu sync.Mutex
	var missed []MissedAlert
	recordMissed := func(alert MissedAlert) {
		mu.Lock()
		missed = append(missed, alert)
		mu.Unlock()
	}

	manager := NewManager(Options{Now: clock.Now, Say: rec.SayText, Logger: logger})
	manager.SetMissedHook(recordMissed)
	t.Cleanup(manager.Close)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if _, err := manager.Set(context.Background(), SetIn{Hour: 7, Schedule: "daily", Label: "wake"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Both fire attempts fail, and so does the best-effort advance; only the
	// late terminal write reaches the store.
	setFireTx(manager, func(context.Context, string, bool) (fireState, bool, error) {
		return fireState{}, false, errFireStore
	})
	setAdvanceFire(manager, func(context.Context, Alarm, *time.Time) error {
		return errFireStore
	})

	waitSaid(t, rec)
	waitFireHistory(t, manager, 1)
	manager.Close()

	// Restart past the downtime grace but before tomorrow's occurrence.
	clock.Set(clock.Now().Add(5 * time.Minute))
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	restarted := NewManager(Options{Now: clock.Now, Logger: logger})
	restarted.SetMissedHook(recordMissed)
	t.Cleanup(restarted.Close)
	if err := restarted.AttachStore(st, nil); err != nil {
		t.Fatalf("restart AttachStore: %v", err)
	}

	mu.Lock()
	falseMisses := append([]MissedAlert(nil), missed...)
	mu.Unlock()
	if len(falseMisses) != 0 {
		t.Fatalf("restart pushed a false missed Notification: %+v", falseMisses)
	}
	events, err := restarted.History(20)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, event := range events {
		if event.Kind == "alarm" && event.State == alertstore.StateMissed {
			t.Fatalf("restart filed a missed row beside the late fire: %+v", event)
		}
	}
	list := restarted.List()
	if len(list) != 1 || !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("List = %+v, want one Alarm next at %s", list, wantNext)
	}
}

// TestDegradedFireRingsWithinThePreRingBudget pins the bound in front of the
// Ring: a store that never answers must not delay a wake-up by one timeout per
// write. The callback is scheduled 500 ms out, so the Ring must start within
// that plus the shared budget (#173).
func TestDegradedFireRingsWithinThePreRingBudget(t *testing.T) {
	st := openFireStore(t)
	rec := newFireSay()
	manager, _ := newFireManager(t, st, rec)

	if _, err := manager.Set(context.Background(), SetIn{Hour: 7, Schedule: "daily", Label: "wake"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	started := time.Now()

	// A wedged store: every write blocks until its context is done.
	setFireTx(manager, func(ctx context.Context, _ string, _ bool) (fireState, bool, error) {
		<-ctx.Done()
		return fireState{}, false, ctx.Err()
	})
	setAdvanceFire(manager, func(ctx context.Context, _ Alarm, _ *time.Time) error {
		<-ctx.Done()
		return ctx.Err()
	})

	select {
	case <-rec.said:
	case <-time.After(5 * time.Second):
		t.Fatal("the Alarm never rang through a wedged store")
	}
	bound := 500*time.Millisecond + firePreRingBudget + 1500*time.Millisecond
	if elapsed := time.Since(started); elapsed > bound {
		t.Fatalf("the Ring started %s after Set, want it bounded by %s", elapsed, bound)
	}
}
