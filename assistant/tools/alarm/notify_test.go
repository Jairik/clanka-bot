package alarm_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// alarmFakeNotifier records the module's missed reports without touching ntfy.
type alarmFakeNotifier struct {
	mu     sync.Mutex
	missed []notify.MissedAlert
	ch     chan notify.MissedAlert
}

func newAlarmFakeNotifier() *alarmFakeNotifier {
	return &alarmFakeNotifier{ch: make(chan notify.MissedAlert, 8)}
}

func (*alarmFakeNotifier) Send(context.Context, notify.Notification) error { return nil }

func (*alarmFakeNotifier) NotifyReminder(context.Context, string) {}

func (f *alarmFakeNotifier) NotifyMissed(_ context.Context, alert notify.MissedAlert) {
	f.mu.Lock()
	f.missed = append(f.missed, alert)
	f.mu.Unlock()
	f.ch <- alert
}

func (f *alarmFakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.missed)
}

func startAlarmModule(t *testing.T, st *store.Store, notifier *alarmFakeNotifier) *alarm.Module {
	t.Helper()
	module := alarm.NewModule()
	deps := tool.Deps{
		Store:  st,
		Notify: notifier,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := module.Register(tool.NewRegistry(time.Second), deps); err != nil {
		t.Fatalf("register alarm module: %v", err)
	}
	if err := module.Start(context.Background()); err != nil {
		t.Fatalf("start alarm module: %v", err)
	}
	return module
}

// TestModuleNotifiesRecoveredMissedAlarm proves the alarm module installs the
// shared Notify hook before Start's recovery, and a restart does not repeat it.
func TestModuleNotifiesRecoveredMissedAlarm(t *testing.T) {
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
		 VALUES ('alarm', '3', 'work', 60000, ?, ?, 'ringing')`,
		now.Add(-time.Minute).UnixMilli(), now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("insert open alarm event: %v", err)
	}
	notifier := newAlarmFakeNotifier()
	module := startAlarmModule(t, st, notifier)
	if module.Manager() == nil {
		t.Fatal("module has no Manager after Register")
	}
	select {
	case alert := <-notifier.ch:
		if alert.Kind != "alarm" || alert.Label != "work" || alert.Reason != "restart" {
			t.Fatalf("alert = %+v", alert)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the alarm module never reported the recovered missed row")
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

	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	second := newAlarmFakeNotifier()
	module = startAlarmModule(t, st, second)
	defer module.Manager().Close()
	select {
	case alert := <-second.ch:
		t.Fatalf("a recovered alarm was notified twice: %+v", alert)
	case <-time.After(200 * time.Millisecond):
	}
	if second.count() != 0 {
		t.Fatalf("second start sent %d notifications", second.count())
	}
}
