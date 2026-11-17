package spotify

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// fakeWatchPlayer is a controllable Player for testing WatchCompletion.
type fakeWatchPlayer struct {
	mu         sync.Mutex
	playing    bool
	err        error
	probeCalls int
	playCalls  int
}

// Play records the call and marks playback active.
func (p *fakeWatchPlayer) Play(_ context.Context, _ spotify.PlayRequest) (spotify.Played, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playCalls++
	p.playing = true
	p.err = nil
	return spotify.Played{Name: "Track"}, nil
}

// Pause marks playback inactive.
func (p *fakeWatchPlayer) Pause(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playing = false
	return nil
}

// Resume marks playback active.
func (p *fakeWatchPlayer) Resume(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playing = true
	return nil
}

// Next marks playback active.
func (p *fakeWatchPlayer) Next(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playing = true
	return nil
}

// SetVolume is an unused stub for Player conformance.
func (p *fakeWatchPlayer) SetVolume(_ context.Context, percent int) (int, bool, error) {
	return percent, false, nil
}

// AdjustVolume is an unused stub for Player conformance.
func (p *fakeWatchPlayer) AdjustVolume(_ context.Context, _ int) (int, bool, error) {
	return 50, false, nil
}

// NowPlaying reports the mock playback state or error.
func (p *fakeWatchPlayer) NowPlaying(_ context.Context) (spotify.NowPlaying, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probeCalls++
	if p.err != nil {
		return spotify.NowPlaying{}, p.err
	}
	return spotify.NowPlaying{Playing: p.playing}, nil
}

// Cap reports the maximum volume.
func (p *fakeWatchPlayer) Cap() int { return 100 }

// probeCount safely returns the total number of NowPlaying calls.
func (p *fakeWatchPlayer) probeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probeCalls
}

// fakeTicker provides a controllable ticker seam recording scheduled intervals.
type fakeTicker struct {
	mu        sync.Mutex
	ch        chan time.Time
	intervals []time.Duration
	created   chan time.Duration
	stopped   int
}

// newFakeTicker builds a test ticker seam.
func newFakeTicker() *fakeTicker {
	return &fakeTicker{
		ch:      make(chan time.Time, 1),
		created: make(chan time.Duration, 10),
	}
}

// factory constructs a channel for a scheduled interval.
func (f *fakeTicker) factory(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	f.intervals = append(f.intervals, d)
	f.mu.Unlock()
	f.created <- d
	return f.ch, func() {
		f.mu.Lock()
		f.stopped++
		f.mu.Unlock()
	}
}

// tick delivers a tick event to the active channel.
func (f *fakeTicker) tick() {
	f.ch <- time.Now()
}

// TestWatchCompletionIntervalBackoffAndWake verifies 2s interval while playing,
// 30s interval when paused, and that a Service play call wakes the watcher.
func TestWatchCompletionIntervalBackoffAndWake(t *testing.T) {
	player := &fakeWatchPlayer{playing: true}
	reg := focus.New()
	svc := NewService(player, reg)
	tick := newFakeTicker()
	svc.newTicker = tick.factory

	svc.takeFocus()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go svc.WatchCompletion(ctx)

	// Watcher starts with active polling (2s).
	select {
	case d := <-tick.created:
		if d != 2*time.Second {
			t.Fatalf("initial interval = %v, want 2s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial ticker")
	}

	// Trigger tick while playing -> remains 2s, probe called.
	tick.tick()
	time.Sleep(10 * time.Millisecond)
	if calls := player.probeCount(); calls < 1 {
		t.Fatalf("probe calls = %d, want at least 1", calls)
	}

	// Pause playback: next tick backs off to 30s.
	player.Pause(ctx)
	tick.tick()

	select {
	case d := <-tick.created:
		if d != 30*time.Second {
			t.Fatalf("paused interval = %v, want 30s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for paused ticker backoff")
	}

	// A Service play call wakes the watcher and resets interval to 2s.
	playTool := NewPlay(svc)
	probesBefore := player.probeCount()
	if _, err := playTool.Handler(ctx, PlayIn{Query: "song"}); err != nil {
		t.Fatalf("play tool: %v", err)
	}

	select {
	case d := <-tick.created:
		if d != 2*time.Second {
			t.Fatalf("woken interval = %v, want 2s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for wake ticker reset")
	}

	// Waking runs an immediate probe without waiting for a ticker tick.
	time.Sleep(10 * time.Millisecond)
	if player.probeCount() <= probesBefore {
		t.Fatalf("probe calls after play = %d, want > %d", player.probeCount(), probesBefore)
	}
}

// TestWatchCompletionRateLimitSuspension verifies that a 429 response with
// Retry-After: 7 suspends probes for 7 seconds.
func TestWatchCompletionRateLimitSuspension(t *testing.T) {
	player := &fakeWatchPlayer{
		playing: true,
		err: &spotify.APIError{
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: 7 * time.Second,
		},
	}
	reg := focus.New()
	svc := NewService(player, reg)
	tick := newFakeTicker()
	svc.newTicker = tick.factory

	svc.takeFocus()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go svc.WatchCompletion(ctx)

	// Wait for initial ticker creation (2s).
	select {
	case d := <-tick.created:
		if d != 2*time.Second {
			t.Fatalf("initial interval = %v, want 2s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial ticker")
	}

	// First tick probes and receives 429 with Retry-After: 7s.
	tick.tick()

	select {
	case d := <-tick.created:
		if d != 7*time.Second {
			t.Fatalf("rate limit suspension interval = %v, want 7s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for 429 suspension ticker")
	}

	// While suspended for 7s, no further probes occur without a tick.
	probes := player.probeCount()
	time.Sleep(20 * time.Millisecond)
	if player.probeCount() != probes {
		t.Fatalf("probes during suspension = %d, want %d", player.probeCount(), probes)
	}

	// When 7s tick fires, probing resumes; clearing 429 returns interval to 2s.
	player.mu.Lock()
	player.err = nil
	player.playing = true
	player.mu.Unlock()

	tick.tick()

	select {
	case d := <-tick.created:
		if d != 2*time.Second {
			t.Fatalf("resumed interval = %v, want 2s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for post-suspension ticker")
	}
}

// TestWatchCompletionSpokenResumeAndNextWakeWatcher verifies that spoken resume
// and next through Focus or Tools wake the watcher.
func TestWatchCompletionSpokenResumeAndNextWakeWatcher(t *testing.T) {
	player := &fakeWatchPlayer{playing: false}
	reg := focus.New()
	svc := NewService(player, reg)
	tick := newFakeTicker()
	svc.newTicker = tick.factory

	svc.takeFocus()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go svc.WatchCompletion(ctx)

	// Consume initial 2s ticker.
	<-tick.created

	// Tick once while paused to enter 30s backoff.
	tick.tick()
	select {
	case d := <-tick.created:
		if d != 30*time.Second {
			t.Fatalf("interval = %v, want 30s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backoff")
	}

	// Spoken resume through Focus wakes the watcher.
	h := holder{s: svc}
	if _, ok := h.HandleIntent(ctx, focus.Resume); !ok {
		t.Fatal("HandleIntent(Resume) failed")
	}

	select {
	case d := <-tick.created:
		if d != 2*time.Second {
			t.Fatalf("woken interval = %v, want 2s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resume wake")
	}

	// Back off to 30s again.
	player.Pause(ctx)
	tick.tick()
	<-tick.created

	// Spoken next through NewNext tool wakes the watcher.
	nextTool := NewNext(svc)
	if _, err := nextTool.Handler(ctx, NextIn{}); err != nil {
		t.Fatalf("nextTool: %v", err)
	}

	select {
	case d := <-tick.created:
		if d != 2*time.Second {
			t.Fatalf("woken interval = %v, want 2s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for next wake")
	}
}

// TestPauseKeepsMusicFocusClaim verifies that pausing playback does not release
// the Music Focus claim.
func TestPauseKeepsMusicFocusClaim(t *testing.T) {
	player := &fakeWatchPlayer{playing: true}
	reg := focus.New()
	svc := NewService(player, reg)
	svc.takeFocus()

	if !reg.Held(focus.Music) {
		t.Fatal("Music Focus should be held")
	}

	pauseTool := NewPause(svc)
	if _, err := pauseTool.Handler(t.Context(), PauseIn{}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Focus claim must remain held after pause.
	if !reg.Held(focus.Music) {
		t.Fatal("Music Focus claim was dropped on pause; want retained")
	}

	// Focus intent pause must also keep claim.
	h := holder{s: svc}
	if _, ok := h.HandleIntent(t.Context(), focus.Pause); !ok {
		t.Fatal("HandleIntent(Pause) failed")
	}
	if !reg.Held(focus.Music) {
		t.Fatal("Music Focus claim was dropped on focus.Pause; want retained")
	}
}
