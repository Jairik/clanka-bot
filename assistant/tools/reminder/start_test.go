package reminder_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// TestModuleStartRecoversAfterSeamsAreBound is the lifecycle contract: the
// module must not recover at Register, before main has bound the live Cues,
// Quiet volume, Zone and speaker. StartAll calls Start for exactly this.
func TestModuleStartRecoversAfterSeamsAreBound(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	now := time.Now()
	if _, err := st.DB().Exec(
		`INSERT INTO reminders (message, kind, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"check the oven", "duration", int64(time.Minute/time.Millisecond),
		now.Add(-30*time.Second).UnixMilli(), now.Add(-90*time.Second).UnixMilli()); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}

	rec := newTraceRecorder()
	queue := announce.New(rec, testLogger())
	defer queue.Close()
	module := reminder.NewModule()
	registry := tool.NewRegistry(time.Second)
	if err := module.Register(registry, tool.Deps{
		Clock:         func() time.Time { return now },
		Logger:        testLogger(),
		Store:         st,
		Speaker:       rec,
		Announcements: queue,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer module.Close()

	// Register must not recover: the reminder is overdue but must stay silent
	// until Start is called with the seams bound.
	rec.expectSilence(t, 100*time.Millisecond)
	if got := module.Manager().List(); len(got) != 0 {
		t.Fatalf("Register recovered %d reminders before Start", len(got))
	}

	if err := module.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the recovered reminder", got)
	}
}
