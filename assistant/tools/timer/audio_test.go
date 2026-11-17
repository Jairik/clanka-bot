package timer_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// Completion media survives editing and restart without changing the deadline.
func TestCompletionAudioPersistence(t *testing.T) {
	st := openTimerStore(t, filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	m := storedManager(t, st, time.Now, nil, nil)
	started, err := m.Start("music", time.Hour, "first song")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetAudio(t.Context(), started.ID, "playlist: running"); err != nil {
		t.Fatal(err)
	}
	m.Close()
	next := storedManager(t, st, time.Now, nil, nil)
	list := next.List()
	if len(list) != 1 || list[0].Audio != "playlist: running" || !list[0].FiresAt.Equal(started.FiresAt.Truncate(time.Millisecond)) {
		t.Fatalf("recovered=%+v", list)
	}
	in, ok := timer.NewSet(next).Extract("set a timer for 15 minutes and play 99 Luftballons")
	if !ok || in.DurationSeconds != 900 || in.Audio != "99 Luftballons" {
		t.Fatalf("extract=%+v %v", in, ok)
	}
}

// TestSetAudioCancelledContext asserts SetAudio returns promptly with an error
// when called with a cancelled or short-deadline context without blocking.
func TestSetAudioCancelledContext(t *testing.T) {
	st := openTimerStore(t, filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	m := storedManager(t, st, time.Now, nil, nil)
	started, err := m.Start("music", time.Hour, "first song")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = m.SetAudio(ctx, started.ID, "playlist: running")
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}

	list := m.List()
	if len(list) != 1 || list[0].Audio != "first song" {
		t.Fatalf("unexpected audio state after cancelled context: %+v", list)
	}
}

// TestSetAudioShortDeadline asserts SetAudio returns promptly with an error
// when called with an expired deadline context.
func TestSetAudioShortDeadline(t *testing.T) {
	st := openTimerStore(t, filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	m := storedManager(t, st, time.Now, nil, nil)
	started, err := m.Start("music", time.Hour, "first song")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Millisecond))
	defer cancel()

	err = m.SetAudio(ctx, started.ID, "playlist: running")
	if err == nil {
		t.Fatal("expected error with expired deadline, got nil")
	}

	list := m.List()
	if len(list) != 1 || list[0].Audio != "first song" {
		t.Fatalf("unexpected audio state after expired context: %+v", list)
	}
}
