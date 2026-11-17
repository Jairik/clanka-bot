package spotify

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	core "github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// TestCommandWakeHonorsRetryAfter checks a command cannot interrupt rate-limit suspension.
func TestCommandWakeHonorsRetryAfter(t *testing.T) {
	player := &fakeWatchPlayer{playing: true, err: &core.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Hour}}
	svc := NewService(player, focus.New())
	tick := newFakeTicker()
	svc.newTicker = tick.factory
	svc.takeFocus()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go svc.WatchCompletion(ctx)
	select {
	case <-tick.created:
	case <-time.After(time.Second):
		t.Fatal("watcher did not schedule its next probe")
	}
	tick.tick()
	select {
	case <-tick.created:
	case <-time.After(time.Second):
		t.Fatal("watcher did not schedule its next probe")
	}
	probes := player.probeCount()
	svc.wakeWatcher()
	time.Sleep(30 * time.Millisecond)
	if player.probeCount() != probes {
		t.Fatalf("probes changed %d -> %d without Retry-After tick", probes, player.probeCount())
	}

	// The retry tick resumes polling after commands were ignored.
	player.mu.Lock()
	player.err = nil
	player.mu.Unlock()
	tick.tick()
	select {
	case interval := <-tick.created:
		if interval != watchActiveInterval || player.probeCount() != probes+1 {
			t.Fatalf("retry did not resume: interval=%v probes=%d", interval, player.probeCount())
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not resume after Retry-After")
	}
}
