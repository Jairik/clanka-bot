package timer_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// fakeNotifier records what the Timer module reports without touching ntfy.
type fakeNotifier struct {
	mu     sync.Mutex
	missed []notify.MissedAlert
	ch     chan notify.MissedAlert
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{ch: make(chan notify.MissedAlert, 8)}
}

func (f *fakeNotifier) Send(context.Context, notify.Notification) error { return nil }

func (f *fakeNotifier) NotifyMissed(_ context.Context, alert notify.MissedAlert) {
	f.mu.Lock()
	f.missed = append(f.missed, alert)
	f.mu.Unlock()
	f.ch <- alert
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.missed)
}

func waitMissed(t *testing.T, notifier *fakeNotifier) notify.MissedAlert {
	t.Helper()
	select {
	case alert := <-notifier.ch:
		return alert
	case <-time.After(3 * time.Second):
		t.Fatal("the Timer module never reported a missed alert")
		return notify.MissedAlert{}
	}
}

// registerTimerModule runs the real module registration, which installs the
// missed hook before AttachStore recovery.
func registerTimerModule(t *testing.T, st *store.Store, notifier *fakeNotifier, engine *ring.Engine) *timer.Module {
	t.Helper()
	module := timer.NewModule()
	deps := tool.Deps{
		Store:  st,
		Notify: notifier,
		Ring:   engine,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := module.Register(tool.NewRegistry(time.Second), deps); err != nil {
		t.Fatalf("register timer module: %v", err)
	}
	return module
}

// TestModuleNotifiesRecoveredMissedRowBeforeScheduling proves the hook is
// installed before recovery: an open row left by a crash reaches Notification
// without waiting for the first live Ring.
func TestModuleNotifiesRecoveredMissedRowBeforeScheduling(t *testing.T) {
	path := t.TempDir() + "/data/assistant.db"
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	now := time.Now()
	if _, err := st.DB().Exec(
		`INSERT INTO alert_events
		 (kind, source_id, label, duration_ms, fires_at, fired_at, state)
		 VALUES ('timer', '7', 'crash pasta', 60000, ?, ?, 'ringing')`,
		now.Add(-time.Minute).UnixMilli(), now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("insert open event: %v", err)
	}

	notifier := newFakeNotifier()
	module := registerTimerModule(t, st, notifier, nil)
	if module.Manager() == nil {
		t.Fatal("module has no Manager after Register")
	}
	alert := waitMissed(t, notifier)
	if alert.Kind != "timer" || alert.Label != "crash pasta" || alert.Reason != "restart" {
		t.Fatalf("alert = %+v", alert)
	}
	if alert.Duration != time.Minute {
		t.Fatalf("duration = %s, want 1m", alert.Duration)
	}
	events, err := module.Manager().History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].State != alertstore.StateMissed || events[0].EndedBy != "restart" {
		t.Fatalf("history = %+v, want one missed/restart row", events)
	}
	module.Manager().Close()
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Restart over the same database: the row is already missed, so the module
	// must not notify it again.
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	second := newFakeNotifier()
	module = registerTimerModule(t, st, second, nil)
	defer module.Manager().Close()
	select {
	case alert := <-second.ch:
		t.Fatalf("a missed row was notified twice across restart: %+v", alert)
	case <-time.After(200 * time.Millisecond):
	}
	if second.count() != 0 {
		t.Fatalf("second start sent %d notifications", second.count())
	}
	events, err = module.Manager().History(10)
	if err != nil {
		t.Fatalf("History after restart: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("history after restart = %+v, want one row", events)
	}
}

// TestModuleNotifiesOldDowntimeTimer covers the retirement path, which writes
// the missed history row and then calls the hook.
func TestModuleNotifiesOldDowntimeTimer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	firesAt := time.Now().Add(-10 * time.Minute)
	if _, err := st.DB().Exec(
		`INSERT INTO timers (label, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?)`,
		"old tea", int64(5*time.Minute/time.Millisecond), firesAt.UnixMilli(), firesAt.Add(-5*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	notifier := newFakeNotifier()
	module := registerTimerModule(t, st, notifier, nil)
	defer module.Manager().Close()
	alert := waitMissed(t, notifier)
	if alert.Label != "old tea" || alert.Reason != "downtime" {
		t.Fatalf("alert = %+v", alert)
	}
	events, err := module.Manager().History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].State != alertstore.StateMissed || events[0].EndedBy != "downtime" {
		t.Fatalf("history = %+v, want missed/downtime", events)
	}
}

// TestModuleNotifiesMaxRingOnceAndNotAgainOnRestart is the end-to-end Timer
// acceptance path: a real Ring times out, the durable row is missed, one
// notification is sent, and a restart does not repeat it.
func TestModuleNotifiesMaxRingOnceAndNotAgainOnRestart(t *testing.T) {
	path := t.TempDir() + "/data/assistant.db"
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	registry := focus.New()
	player := newTestPlayer(false)
	engine := testEngine(registry, player, ring.Settings{
		MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond, SpeakLabel: ring.SpeakNever,
	}, nil, nil)
	notifier := newFakeNotifier()
	module := registerTimerModule(t, st, notifier, engine)
	if module.Manager() == nil {
		t.Fatal("module has no Manager after Register")
	}
	if _, err := module.Manager().Start("pasta", 2*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	alert := waitMissed(t, notifier)
	if alert.Label != "pasta" || alert.Reason != "max_ring" {
		t.Fatalf("alert = %+v", alert)
	}
	waitHistory(t, module.Manager(), 1)
	module.Manager().Close()
	if notifier.count() != 1 {
		t.Fatalf("max_ring sent %d notifications, want one", notifier.count())
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	second := newFakeNotifier()
	module = registerTimerModule(t, st, second, nil)
	defer module.Manager().Close()
	select {
	case alert := <-second.ch:
		t.Fatalf("a max_ring missed row was notified twice: %+v", alert)
	case <-time.After(200 * time.Millisecond):
	}
}
