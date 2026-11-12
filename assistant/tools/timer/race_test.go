package timer_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// TestConcurrentSetCancelAndFire exercises the scheduler and cancel paths
// together. Run it with -race; the assertions are narrow on purpose: a Timer
// cancelled while it is still pending must never speak, a Timer cancelled
// while its Ring is sounding must be stopped, and the Manager must not panic
// or deadlock.
func TestConcurrentSetCancelAndFire(t *testing.T) {
	var spoken atomic.Int64
	say := func(context.Context, string) error {
		spoken.Add(1)
		return nil
	}
	m := timer.NewManager(nil, say, slog.Default())
	defer m.Close()

	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tm, err := m.Start("", time.Millisecond)
			if err != nil {
				t.Errorf("Start: %v", err)
				return
			}
			if i%2 == 0 {
				if _, err := m.CancelTimers(tm.ID); err != nil {
					t.Errorf("CancelTimers: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	// Let the uncancelled half fire, then stop any straggler. A cancel that
	// raced an already-sounding Ring may count that Ring, which has already
	// spoken; the deterministic batch below covers the never-speak promise.
	deadline := time.Now().Add(2 * time.Second)
	for spoken.Load() < 40 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := m.CancelTimers(""); err != nil {
		t.Fatalf("CancelTimers(all): %v", err)
	}
	if got := len(m.List()); got != 0 {
		t.Fatalf("timers = %d after cancelling everything, want 0", got)
	}

	// A pending Timer cancelled before its deadline never speaks. A fresh
	// Manager keeps the first phase's stragglers out of this assertion.
	var silent atomic.Int64
	m2 := timer.NewManager(nil, func(context.Context, string) error {
		silent.Add(1)
		return nil
	}, slog.Default())
	defer m2.Close()
	for i := 0; i < 10; i++ {
		tm, err := m2.Start("", 50*time.Millisecond)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := m2.CancelTimers(tm.ID); err != nil {
			t.Fatalf("CancelTimers: %v", err)
		}
	}
	time.Sleep(120 * time.Millisecond)
	if got := silent.Load(); got != 0 {
		t.Fatalf("alerts = %d, want the 10 cancelled Timers silent", got)
	}
}

// TestConcurrentRemainingAndListReadsDuringSet checks the snapshot methods
// against live writes; it is mostly a race-detector exercise.
func TestConcurrentRemainingAndListReadsDuringSet(t *testing.T) {
	m := timer.NewManager(nil, nil, slog.Default())
	defer m.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := m.Start("pasta", 5*time.Millisecond); err != nil {
				t.Errorf("Start: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_ = m.List()
		_, _ = m.Remaining("pasta")
	}
	wg.Wait()
	m.Cancel("")
}
