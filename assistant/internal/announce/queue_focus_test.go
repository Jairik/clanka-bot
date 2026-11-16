package announce_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
)

func queueTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSnapshotReportsCounts pins that Snapshot returns only the count and the
// hold/playing flags, with no message content (#217).
func TestSnapshotReportsCounts(t *testing.T) {
	queue := announce.New(nil, queueTestLogger(), announce.WithPlayback())
	t.Cleanup(queue.Close)

	if s := queue.Snapshot(); s.Len != 0 || s.Held || s.Playing {
		t.Fatalf("empty snapshot = %+v, want {0 false false}", s)
	}

	// A hold keeps the queued items from playing, so the count is stable.
	queue.Hold()
	queue.EnqueuePlay(func(context.Context) error { return nil })
	queue.EnqueuePlay(func(context.Context) error { return nil })

	s := queue.Snapshot()
	if s.Len != 2 {
		t.Fatalf("Len = %d, want 2", s.Len)
	}
	if !s.Held {
		t.Fatal("Held = false, want true while a hold is on")
	}
	if s.Playing {
		t.Fatal("Playing = true, want false")
	}
}

// TestOnChangeFiresOnQueueAndHold pins that OnChange runs when an item is
// queued and when a hold is taken, so the Dashboard delta keeps up (#217).
func TestOnChangeFiresOnQueueAndHold(t *testing.T) {
	queue := announce.New(nil, queueTestLogger(), announce.WithPlayback())
	t.Cleanup(queue.Close)

	var calls atomic.Int32
	queue.OnChange = func() { calls.Add(1) }

	if !queue.EnqueuePlay(func(context.Context) error { return nil }) {
		t.Fatal("EnqueuePlay rejected")
	}
	if got := calls.Load(); got < 1 {
		t.Fatalf("OnChange calls after EnqueuePlay = %d, want >= 1", got)
	}
	before := calls.Load()
	queue.Hold()
	if got := calls.Load(); got <= before {
		t.Fatalf("OnChange calls after Hold = %d, want > %d", got, before)
	}
}
