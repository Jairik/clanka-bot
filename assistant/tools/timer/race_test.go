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
// together. Run it with -race; the assertion is narrow on purpose: a cancelled
// Timer must never speak, and the Manager must not panic or deadlock.
func TestConcurrentSetCancelAndFire(t *testing.T) {
	var spoken atomic.Int64
	say := func(context.Context, string) error {
		spoken.Add(1)
		return nil
	}
	m := timer.NewManager(nil, say, slog.Default())
	defer m.Close()

	var (
		wg        sync.WaitGroup
		cancelled atomic.Int64
	)
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
				n, err := m.CancelTimers(tm.ID)
				if err != nil {
					t.Errorf("CancelTimers: %v", err)
					return
				}
				// A timer that fired before this call was not cancelled; only
				// count the calls that actually removed an entry.
				cancelled.Add(int64(n))
			}
		}(i)
	}
	wg.Wait()

	// Let the uncancelled half fire, then stop any straggler.
	deadline := time.Now().Add(2 * time.Second)
	for spoken.Load() < 40 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := m.CancelTimers(""); err != nil {
		t.Fatalf("CancelTimers(all): %v", err)
	}
	if got := spoken.Load(); got > 80-cancelled.Load() {
		t.Fatalf("alerts = %d, want at most the %d timers that were not cancelled", got, 80-cancelled.Load())
	}
	if got := len(m.List()); got != 0 {
		t.Fatalf("timers = %d after cancelling everything, want 0", got)
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
