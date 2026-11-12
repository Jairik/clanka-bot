package tools_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// TestStartAllRecoversAfterSeamsAreBound is the #52 ordering fix: a recovered
// Timer must not Ring during RegisterAll, before main has bound the live
// settings, Cues, Zone and Ring seams. The engine's Settings and Clip closures
// fail the test if they are read before StartAll.
func TestStartAllRecoversAfterSeamsAreBound(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	now := time.Now()
	if _, err := st.DB().Exec(
		`INSERT INTO timers (label, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?)`,
		"recovered", int64(time.Minute/time.Millisecond), now.Add(-time.Minute).UnixMilli(), now.Add(-2*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("seed timer: %v", err)
	}

	var ready atomic.Bool
	played := make(chan struct{}, 16)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := ring.New(ring.Deps{
		Focus: focus.New(),
		Settings: func(ring.Kind) ring.Settings {
			if !ready.Load() {
				t.Error("Ring read settings before StartAll bound them")
			}
			return ring.Settings{MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) {
			if !ready.Load() {
				t.Error("Ring resolved a Cue before StartAll bound it")
			}
			return sound.Clip{
				PCM:    []byte{0, 0, 0, 0},
				Format: audio.Format{Rate: 8000, Width: 2, Channels: 1},
			}, true
		},
		Play: func(context.Context, sound.Clip) error {
			played <- struct{}{}
			return nil
		},
		Logger: logger,
	})

	set := tools.NewSet()
	deps := tool.Deps{
		Clock:  func() time.Time { return now },
		Logger: logger,
		Store:  st,
		Focus:  focus.New(),
		Ring:   engine,
	}
	if err := set.RegisterAll(tool.NewRegistry(time.Second), deps, nil); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	select {
	case <-played:
		t.Fatal("a recovered Timer rang during RegisterAll, before the live seams were bound")
	case <-time.After(50 * time.Millisecond):
	}
	if module, ok := set.Module("timer").(*timer.Module); ok {
		if manager := module.Manager(); manager != nil {
			defer manager.Close()
		}
	}

	ready.Store(true)
	if err := set.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	select {
	case <-played:
	case <-time.After(2 * time.Second):
		t.Fatal("the recovered Timer never rang after StartAll")
	}
}
