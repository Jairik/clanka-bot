package timer_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// ringRecorder blocks one raw Ring per label until its context is cancelled.
type ringRecorder struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	stopped map[string]chan struct{}
}

func newRingRecorder() *ringRecorder {
	return &ringRecorder{started: map[string]chan struct{}{}, stopped: map[string]chan struct{}{}}
}

func (r *ringRecorder) ring(ctx context.Context, tm timer.Timer) bool {
	label := tm.Label
	started, stopped := r.channels(label)
	close(started)
	select {
	case <-ctx.Done():
		close(stopped)
		return true
	case <-time.After(3 * time.Second):
		return true
	}
}

// channels returns this label's one-shot started/stopped pair, creating both
// under the lock so concurrent Rings cannot race on the maps.
func (r *ringRecorder) channels(label string) (started, stopped chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started[label] == nil {
		r.started[label] = make(chan struct{})
	}
	if r.stopped[label] == nil {
		r.stopped[label] = make(chan struct{})
	}
	return r.started[label], r.stopped[label]
}

func (r *ringRecorder) channel(set string, label string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	var channels map[string]chan struct{}
	if set == "started" {
		channels = r.started
	} else {
		channels = r.stopped
	}
	if channels[label] == nil {
		channels[label] = make(chan struct{})
	}
	return channels[label]
}

func (r *ringRecorder) waitStarted(t *testing.T, label string) {
	t.Helper()
	select {
	case <-r.channel("started", label):
	case <-time.After(2 * time.Second):
		t.Fatalf("timer %q never started ringing", label)
	}
}

func (r *ringRecorder) waitStopped(t *testing.T, label string) {
	t.Helper()
	select {
	case <-r.channel("stopped", label):
	case <-time.After(2 * time.Second):
		t.Fatalf("timer %q kept ringing", label)
	}
}

func TestCancelLabelStopsARingingTimer(t *testing.T) {
	ringing := newRingRecorder()
	m := timer.NewManager(nil, nil, nil)
	m.SetRing(ringing.ring)
	t.Cleanup(m.Close)
	if _, err := m.Start("pasta", 10*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ringing.waitStarted(t, "pasta")

	if cancelled, err := m.CancelLabel("pasta"); err != nil || cancelled != 1 {
		t.Fatalf("CancelLabel = %d, %v; want the ringing Timer counted", cancelled, err)
	}
	ringing.waitStopped(t, "pasta")

	// The Ring is gone, so a second cancel is not counted.
	if cancelled, err := m.CancelLabel("pasta"); err != nil || cancelled != 0 {
		t.Fatalf("second CancelLabel = %d, %v", cancelled, err)
	}
}

func TestCancelLabelLeavesANewerUnrelatedRing(t *testing.T) {
	ringing := newRingRecorder()
	m := timer.NewManager(nil, nil, nil)
	m.SetRing(ringing.ring)
	t.Cleanup(m.Close)
	if _, err := m.Start("pasta", 10*time.Millisecond); err != nil {
		t.Fatalf("Start pasta: %v", err)
	}
	if _, err := m.Start("tea", 10*time.Millisecond); err != nil {
		t.Fatalf("Start tea: %v", err)
	}
	ringing.waitStarted(t, "pasta")
	ringing.waitStarted(t, "tea")

	if cancelled, err := m.CancelLabel("pasta"); err != nil || cancelled != 1 {
		t.Fatalf("CancelLabel = %d, %v", cancelled, err)
	}
	ringing.waitStopped(t, "pasta")

	select {
	case <-ringing.channel("stopped", "tea"):
		t.Fatal("cancelling pasta also stopped the unrelated tea Ring")
	case <-time.After(100 * time.Millisecond):
	}
	if cancelled := m.Cancel(""); cancelled != 1 {
		t.Fatalf("Cancel all = %d, want the tea Ring", cancelled)
	}
	ringing.waitStopped(t, "tea")
}

// TestCancelWinsAgainstMidFireTimer proves a cancel that lands while a Timer is
// mid-fire (its pending row is gone, its active Ring is registered, but the
// history transaction has not finished and the Ring has not started) is counted
// and stops the alert. It blocks the fire at a precise point with the test
// gate hook rather than racing on a sleep, so it is deterministic.
func TestCancelWinsAgainstMidFireTimer(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	defer st.Close()
	ringing := newRingRecorder()
	m := storedManager(t, st, nil, nil, nil)
	m.SetRing(ringing.ring)

	reached := make(chan string, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	m.SetFireGateHook(func(id string) {
		reached <- id
		<-release
	})

	timer, err := m.Start("pasta", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for fire to reach the gate: the live entry is deleted and the active
	// Ring is registered, but the Ring has not started and the transaction may
	// not have finished.
	select {
	case gateID := <-reached:
		if gateID != timer.ID {
			t.Fatalf("gate fired for %q, want %q", gateID, timer.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fire never reached the mid-fire gate")
	}

	// The pending row is gone and the read Tools can no longer see it, but a
	// cancel must still find and stop the in-flight Timer.
	cancelled, err := m.CancelTimers(timer.ID)
	if err != nil {
		t.Fatalf("CancelTimers: %v", err)
	}
	if cancelled != 1 {
		t.Fatalf("CancelTimers = %d, want 1 (the mid-fire Timer)", cancelled)
	}

	// Let the fire goroutine finish the transaction and observe the cancel.
	closeRelease()

	// The Ring must never start now that the fire was cancelled mid-flight.
	select {
	case <-ringing.channel("started", "pasta"):
		t.Fatal("Ring started despite a mid-fire cancel")
	case <-time.After(200 * time.Millisecond):
	}

	// The history row ends as stopped/cancel rather than fired/ringing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := m.History(1)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(events) == 1 && events[0].State == alertstore.StateStopped && events[0].EndedBy == "cancel" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the mid-fire cancel never recorded a stopped/cancel history row")
}

func TestCancelLabelRecordsAStoppedHistoryRow(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	defer st.Close()
	ringing := newRingRecorder()
	m := storedManager(t, st, nil, nil, nil)
	m.SetRing(ringing.ring)
	if _, err := m.Start("pasta", 10*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ringing.waitStarted(t, "pasta")
	if cancelled, err := m.CancelLabel("pasta"); err != nil || cancelled != 1 {
		t.Fatalf("CancelLabel = %d, %v", cancelled, err)
	}
	ringing.waitStopped(t, "pasta")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := m.History(1)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(events) == 1 && events[0].State == alertstore.StateStopped {
			if events[0].EndedBy != "cancel" {
				t.Fatalf("ended_by = %q, want cancel", events[0].EndedBy)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the cancelled Ring never recorded a stopped/cancel history row")
}
