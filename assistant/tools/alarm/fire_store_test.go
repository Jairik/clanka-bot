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
func setFireTx(manager *Manager, fn func(id string, snoozed bool) (fireState, bool, error)) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.fireTx = fn
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
	setFireTx(manager, func(id string, snoozed bool) (fireState, bool, error) {
		if calls.Add(1) == 1 {
			return fireState{}, false, errFireStore
		}
		return manager.beginFire(id, snoozed)
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
			setFireTx(manager, func(string, bool) (fireState, bool, error) {
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
