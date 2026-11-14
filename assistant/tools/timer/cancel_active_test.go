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
