package timer_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// fastClock advances on Sleep, so max_ring and gap complete without wall time.
type fastClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFastClock() *fastClock { return &fastClock{now: time.Now()} }

func (c *fastClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fastClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

// testPlayer records each pass. In block mode it holds until the Ring context
// ends, which keeps a Ring live for the stop and queue tests.
type testPlayer struct {
	mu      sync.Mutex
	starts  int
	started chan struct{}
	block   bool
}

func newTestPlayer(block bool) *testPlayer {
	return &testPlayer{started: make(chan struct{}, 8), block: block}
}

func (p *testPlayer) Play(ctx context.Context, _ sound.Clip) error {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	p.started <- struct{}{}
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (p *testPlayer) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// testEngine builds a Ring engine over fixed settings and the real store's
// Focus registry. It returns the engine plus a settings setter for one test.
func testEngine(registry *focus.Registry, player *testPlayer, settings ring.Settings, speaker ring.Speaker, music ring.Music) *ring.Engine {
	if settings.MaxRing == 0 {
		settings = ring.Settings{
			MaxRing: time.Hour, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
		}
	}
	return ring.New(ring.Deps{
		Focus:    registry,
		Music:    music,
		Speaker:  speaker,
		Settings: func(ring.Kind) ring.Settings { return settings },
		Clip: func(ring.Kind) (sound.Clip, bool) {
			return sound.Clip{
				PCM:    []byte{0, 0, 0, 0},
				Format: audio.Format{Rate: 8000, Width: 2, Channels: 1},
			}, true
		},
		Play:   player.Play,
		Clock:  newFastClock(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// waitHistory polls until want terminal events are present or fails. A fire
// inserts the open row before the Ring ends, so the test waits for the outcome,
// not only the row.
func waitHistory(t *testing.T, manager *timer.Manager, want int) []alertstore.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := manager.History(20)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		terminal := 0
		for _, event := range events {
			if !event.State.Open() {
				terminal++
			}
		}
		if terminal >= want {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("history has %d terminal events, want %d: %+v", terminal, want, events)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// managerWithStore attaches the real database and the real Focus registry.
func managerWithStore(t *testing.T, st *store.Store, registry *focus.Registry, engine *ring.Engine) *timer.Manager {
	t.Helper()
	manager := timer.NewManager(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFocus(registry)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	manager.SetRingEngine(engine)
	t.Cleanup(manager.Close)
	return manager
}

// TestRingTimeoutRecordsMissed is the max_ring acceptance path: the Ring ends
// on its own, the Timer row is gone and a durable missed row remains.
func TestRingTimeoutRecordsMissed(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(false)
	engine := testEngine(registry, player, ring.Settings{
		MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, nil, nil)
	manager := managerWithStore(t, st, registry, engine)
	missed := make(chan timer.MissedAlert, 1)
	manager.SetMissedHook(func(alert timer.MissedAlert) { missed <- alert })

	if _, err := manager.Start("pasta", 2*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case alert := <-missed:
		if alert.Reason != "max_ring" || alert.Label != "pasta" {
			t.Fatalf("missed alert = %+v", alert)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the missed hook did not run at max_ring")
	}
	events := waitHistory(t, manager, 1)
	event := events[0]
	if event.State != alertstore.StateMissed || event.EndedBy != "timeout" {
		t.Fatalf("event = %+v, want missed/timeout", event)
	}
	if event.Kind != "timer" || event.Label != "pasta" {
		t.Fatalf("event identity = %+v", event)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM timers`).Scan(&rows); err != nil {
		t.Fatalf("count timers: %v", err)
	}
	if rows != 0 {
		t.Fatalf("pending timer rows = %d after firing, want 0", rows)
	}
	if player.startCount() == 0 {
		t.Fatal("the Ring never looped")
	}
}

// TestStopRecordsStoppedAndEndedBy covers stop on the real manager/engine.
func TestStopRecordsStoppedAndEndedBy(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(true)
	engine := testEngine(registry, player, ring.Settings{}, nil, nil)
	manager := managerWithStore(t, st, registry, engine)

	if _, err := manager.Start("tea", 2*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-player.started
	if text, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled || text != timer.StopText {
		t.Fatalf("stop = %q, %v", text, handled)
	}
	events := waitHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "stop" {
		t.Fatalf("event = %+v, want stopped/stop", events[0])
	}
}

// TestSnoozeRecordsSnoozeActingAsStop is the Timer-specific acceptance rule.
func TestSnoozeRecordsSnoozeActingAsStop(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(true)
	engine := testEngine(registry, player, ring.Settings{}, nil, nil)
	manager := managerWithStore(t, st, registry, engine)

	if _, err := manager.Start("", 2*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-player.started
	if text, handled := registry.HandleIntent(context.Background(), focus.Snooze); !handled || text != timer.StopText {
		t.Fatalf("snooze = %q, %v; want act as stop", text, handled)
	}
	events := waitHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "snooze" {
		t.Fatalf("event = %+v, want stopped/snooze", events[0])
	}
}

// TestQueuedRingsRecordEachOutcome proves two simultaneous Timers ring one at
// a time and each keeps its own controllable history row.
func TestQueuedRingsRecordEachOutcome(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(true)
	engine := testEngine(registry, player, ring.Settings{}, nil, nil)
	manager := managerWithStore(t, st, registry, engine)

	if _, err := manager.Start("first", 2*time.Millisecond); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	<-player.started
	if _, err := manager.Start("second", 2*time.Millisecond); err != nil {
		t.Fatalf("Start second: %v", err)
	}
	// The second Timer fires while the first still owns the speaker; it must
	// not replace the first's Focus.
	time.Sleep(20 * time.Millisecond)
	if text, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled || text != timer.StopText {
		t.Fatalf("first stop = %q, %v", text, handled)
	}
	select {
	case <-player.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the queued Ring never started")
	}
	if text, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled || text != timer.StopText {
		t.Fatalf("second stop = %q, %v", text, handled)
	}
	events := waitHistory(t, manager, 2)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want both Timers", events)
	}
	for _, event := range events {
		if event.State != alertstore.StateStopped || event.EndedBy != "stop" {
			t.Fatalf("event = %+v, want stopped/stop", event)
		}
	}
	if got := []string{events[0].Label, events[1].Label}; got[0] == got[1] {
		t.Fatalf("events both label %q", got[0])
	}
}

// TestShutdownRecordsInterruptedRing proves Close waits for the in-flight fire
// and records a clean shutdown distinctly from a missed answer.
func TestShutdownRecordsInterruptedRing(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(true)
	engine := testEngine(registry, player, ring.Settings{}, nil, nil)
	manager := timer.NewManager(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFocus(registry)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	manager.SetRingEngine(engine)
	if _, err := manager.Start("shutdown", 2*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-player.started
	manager.Close()
	events, err := manager.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].State != alertstore.StateInterrupted || events[0].EndedBy != "shutdown" {
		t.Fatalf("history = %+v, want interrupted/shutdown", events)
	}
}

// TestCrashOpenEventBecomesMissedOnRestart simulates a crash after the fire
// transaction but before an outcome, then proves the next start resolves it.
func TestCrashOpenEventBecomesMissedOnRestart(t *testing.T) {
	path := t.TempDir() + "/data/assistant.db"
	ctx := context.Background()
	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	now := time.Now()
	if _, err := first.DB().Exec(
		`INSERT INTO alert_events
		 (kind, source_id, label, duration_ms, fires_at, fired_at, state)
		 VALUES ('timer', '9', 'crash', 60000, ?, ?, 'ringing')`,
		now.Add(-time.Minute).UnixMilli(), now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("insert open event: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	defer second.Close()
	var registry focus.Registry
	manager := timer.NewManager(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFocus(&registry)
	missed := make(chan timer.MissedAlert, 1)
	manager.SetMissedHook(func(alert timer.MissedAlert) { missed <- alert })
	if err := manager.AttachStore(second, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	defer manager.Close()
	select {
	case alert := <-missed:
		if alert.Label != "crash" || alert.Reason != "restart" {
			t.Fatalf("recovered alert = %+v, want restart/crash", alert)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the startup recovery hook did not run")
	}
	events, err := manager.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].State != alertstore.StateMissed || events[0].EndedBy != "restart" {
		t.Fatalf("history = %+v, want missed/restart", events)
	}
}

// TestDowntimeMissedIsPersisted covers the older-than-grace path: the pending
// row is retired as a missed history row, not only spoken in the summary.
func TestDowntimeMissedIsPersisted(t *testing.T) {
	path := t.TempDir() + "/data/assistant.db"
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	now := time.Now()
	firesAt := now.Add(-10 * time.Minute)
	if _, err := st.DB().Exec(
		`INSERT INTO timers (label, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?)`,
		"old pasta", int64(5*time.Minute/time.Millisecond), firesAt.UnixMilli(), firesAt.Add(-5*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	defer st.Close()

	var registry focus.Registry
	manager := timer.NewManager(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithFocus(&registry)
	missed := make(chan timer.MissedAlert, 1)
	manager.SetMissedHook(func(alert timer.MissedAlert) { missed <- alert })
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	defer manager.Close()
	select {
	case alert := <-missed:
		if alert.Label != "old pasta" || alert.Reason != "downtime" {
			t.Fatalf("downtime alert = %+v", alert)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the downtime missed hook did not run")
	}
	events, err := manager.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].State != alertstore.StateMissed || events[0].EndedBy != "downtime" {
		t.Fatalf("history = %+v, want missed/downtime", events)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM timers`).Scan(&rows); err != nil {
		t.Fatalf("count timers: %v", err)
	}
	if rows != 0 {
		t.Fatalf("overdue timer rows = %d, want 0", rows)
	}
}
