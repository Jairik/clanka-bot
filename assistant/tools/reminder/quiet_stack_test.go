package reminder_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// peakSink records the loudest PCM sample written through the real sound
// service, which is enough to prove the Quiet-hours Cue gain was applied.
type peakSink struct {
	mu   sync.Mutex
	peak int
}

func (s *peakSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return &peakWriter{sink: s}, nil
}

func (s *peakSink) Peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

type peakWriter struct{ sink *peakSink }

func (w *peakWriter) Write(p []byte) (int, error) {
	peak := pcmPeak(p)
	w.sink.mu.Lock()
	if peak > w.sink.peak {
		w.sink.peak = peak
	}
	w.sink.mu.Unlock()
	return len(p), nil
}

func (w *peakWriter) Close() error { return nil }

// TestQuietReminderUsesTheRealSoundStack composes the production pieces: a
// real sound library, the real Quiet service over a real settings database,
// the Announcement queue and the Manager. It proves the live Cue is scaled,
// the message carries quiet.volume, and the ordinary item stays held.
func TestQuietReminderUsesTheRealSoundStack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	format := audio.Format{Rate: 16000, Width: 2, Channels: 1}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cues, err := sound.New(ctx, st, t.TempDir(), format, logger)
	if err != nil {
		t.Fatalf("sound.New: %v", err)
	}
	defs := append(cues.Definitions(sound.Config{}), quiet.Definitions(quiet.Config{
		Enabled: true, Start: "00:00", End: "23:59", Volume: 20,
	})...)
	sett, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	sink := &peakSink{}
	cues.Bind(sett, sink)
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	quietSvc := quiet.New(quiet.Options{Reader: sett, Now: func() time.Time { return now }, Location: func() *time.Location { return time.UTC }})
	quietSvc.BindWriter(sett)
	if !quietSvc.IsQuiet() {
		t.Fatal("test window is not quiet")
	}

	clip, ok := cues.Clip(sound.RoleReminder)
	if !ok {
		t.Fatal("reminder Cue did not resolve")
	}
	normalPeak := pcmPeak(clip.PCM)
	if normalPeak == 0 {
		t.Fatal("built-in reminder Cue has no audible sample")
	}

	rec := newTraceRecorder()
	queue := announce.New(rec, logger)
	defer queue.Close()
	manager := reminder.NewManager(reminder.Options{
		Now: func() time.Time { return now }, Speaker: rec, Queue: queue,
		Cues: cues, Quiet: quietSvc, Logger: logger,
	})
	defer manager.Close()

	queue.Hold()
	queue.Enqueue("The bins go out tonight.")
	if _, err := manager.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q", got)
	}
	if gain := rec.lastGain(); gain != 20 {
		t.Fatalf("speech gain = %d, want quiet.volume 20", gain)
	}
	quietPeak := sink.Peak()
	if quietPeak < normalPeak*15/100 || quietPeak > normalPeak*25/100 {
		t.Fatalf("quiet Cue peak = %d, want about 20%% of %d", quietPeak, normalPeak)
	}
	rec.expectSilence(t, 100*time.Millisecond)
	queue.Release()
	if got := rec.wait(t, 2*time.Second); got != "The bins go out tonight." {
		t.Fatalf("held announcement = %q", got)
	}
}
