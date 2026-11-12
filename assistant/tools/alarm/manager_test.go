package alarm_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// fixedClock is the manager's injected time source. Tests choose a wall time
// just before the Alarm so the real AfterFunc delay stays tiny.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// fastClock advances on Sleep so a Ring's max_ring and gap finish without wall
// time. It is the Ring engine's clock, not the scheduler's.
type fastClock struct {
	mu  sync.Mutex
	now time.Time
}

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

// ringPlayer records starts and lets a test react to each pass. The callback
// runs on the Ring goroutine, so it can deliver a Focus intent exactly when the
// Alarm owns the speaker.
type ringPlayer struct {
	mu      sync.Mutex
	starts  int
	started chan struct{}
	onPlay  func(pass int, ctx context.Context) error
}

func newRingPlayer(onPlay func(int, context.Context) error) *ringPlayer {
	return &ringPlayer{started: make(chan struct{}, 16), onPlay: onPlay}
}

func (p *ringPlayer) Play(ctx context.Context, _ sound.Clip) error {
	p.mu.Lock()
	p.starts++
	pass := p.starts
	p.mu.Unlock()
	p.started <- struct{}{}
	if p.onPlay != nil {
		return p.onPlay(pass, ctx)
	}
	return nil
}

func (p *ringPlayer) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// testEngine builds a Ring engine over the real Focus registry and a fast
// clock. The max_ring is long because the tests stop or snooze the Ring
// themselves; a timeout test builds its own engine with a short max_ring.
func testEngine(registry *focus.Registry, player *ringPlayer) *ring.Engine {
	return ring.New(ring.Deps{
		Focus: registry,
		Settings: func(ring.Kind) ring.Settings {
			return ring.Settings{
				MaxRing: time.Hour,
				Volume:  100,
				Gap:     time.Millisecond,
				Snooze:  30 * time.Millisecond,
			}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) {
			return sound.Clip{
				PCM:    []byte{0, 0, 0, 0},
				Format: audio.Format{Rate: 8000, Width: 2, Channels: 1},
			}, true
		},
		Play:   player.Play,
		Clock:  &fastClock{now: time.Now()},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// sayRecorder collects spoken lines.
type sayRecorder struct {
	texts chan string
}

func newSayRecorder() *sayRecorder { return &sayRecorder{texts: make(chan string, 16)} }

func (r *sayRecorder) SayText(_ context.Context, text string) error {
	r.texts <- text
	return nil
}

// openStore opens the real database with the real migrations.
func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// newManager builds a Manager with the test seams and closes it at cleanup.
func newManager(t *testing.T, clock *fixedClock, say func(context.Context, string) error, q *announce.Queue, snooze func() time.Duration) *alarm.Manager {
	t.Helper()
	manager := alarm.NewManager(alarm.Options{
		Now:           clock.Now,
		Say:           say,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Announcements: q,
		Snooze:        snooze,
	})
	t.Cleanup(manager.Close)
	return manager
}

// waitAlarmHistory waits for want terminal Alarm events.
func waitAlarmHistory(t *testing.T, manager *alarm.Manager, want int) []alertstore.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := manager.History(20)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		terminal := 0
		for _, event := range events {
			if event.Kind == "alarm" && !event.State.Open() {
				terminal++
			}
		}
		if terminal >= want {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("alarm history has %d terminal events, want %d: %+v", terminal, want, events)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// countAlarmRows counts persisted Alarms.
func countAlarmRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&count); err != nil {
		t.Fatalf("count alarms: %v", err)
	}
	return count
}

// TestOneOffFiresAndIsRemoved covers the simplest durable path: a one-off
// Alarm Rings once through the fallback line and its row is gone afterwards.
func TestOneOffFiresAndIsRemoved(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	rec := newSayRecorder()
	manager := newManager(t, clock, rec.SayText, nil, nil)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	created, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Label: "wake"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if created.Schedule != alarm.Once || !created.NextFiresAt.Equal(time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("created = %+v, want a one-off at 07:00", created)
	}
	select {
	case text := <-rec.texts:
		if text != "Wake alarm is going off." {
			t.Fatalf("alert = %q, want the labelled fallback", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the one-off Alarm did not fire")
	}
	events := waitAlarmHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "spoken" {
		t.Fatalf("event = %+v, want stopped/spoken", events[0])
	}
	if rows := countAlarmRows(t, st); rows != 0 {
		t.Fatalf("alarm rows = %d after a one-off fire, want 0", rows)
	}
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("List = %+v, want no alarms", list)
	}
}

// TestStopContinuesTheRepeat proves a stopped repeating Alarm schedules its
// next occurrence exactly once.
func TestStopContinuesTheRepeat(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	player := newRingPlayer(func(pass int, ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	manager := newManager(t, clock, nil, nil, nil)
	manager.SetRingEngine(testEngine(registry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case <-player.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Alarm never rang")
	}
	if text, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled || text != "Alarm stopped." {
		t.Fatalf("stop = %q, %v; want Alarm stopped", text, handled)
	}
	events := waitAlarmHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "stop" {
		t.Fatalf("event = %+v, want stopped/stop", events[0])
	}
	list := manager.List()
	if len(list) != 1 {
		t.Fatalf("List = %+v, want the repeating Alarm", list)
	}
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	if !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("next = %s, want %s", list[0].NextFiresAt, wantNext)
	}
	var nextMS int64
	var snoozeMS *int64
	if err := st.DB().QueryRow(`SELECT next_fires_at, snoozed_until FROM alarms`).Scan(&nextMS, &snoozeMS); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if nextMS != wantNext.UnixMilli() || snoozeMS != nil {
		t.Fatalf("row next = %d snooze = %v, want %d and NULL", nextMS, snoozeMS, wantNext.UnixMilli())
	}
}

// TestSnoozeReringsAndKeepsTheRepeat is the Alarm snooze acceptance path: the
// first Ring snoozes, a second fire happens after the delay, and the repeat
// schedule still points at tomorrow.
func TestSnoozeReringsAndKeepsTheRepeat(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	player := newRingPlayer(func(pass int, ctx context.Context) error {
		if pass == 1 {
			if text, handled := registry.HandleIntent(context.Background(), focus.Snooze); !handled || text != "Alarm snoozed." {
				t.Errorf("snooze = %q, %v; want Alarm snoozed", text, handled)
			}
		} else {
			if text, handled := registry.HandleIntent(context.Background(), focus.Stop); !handled || text != "Alarm stopped." {
				t.Errorf("stop = %q, %v; want Alarm stopped", text, handled)
			}
		}
		<-ctx.Done()
		return ctx.Err()
	})
	manager := newManager(t, clock, nil, nil, func() time.Duration { return 30 * time.Millisecond })
	manager.SetRingEngine(testEngine(registry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	events := waitAlarmHistory(t, manager, 2)
	var snoozed, stopped bool
	for _, event := range events {
		if event.Kind != "alarm" {
			continue
		}
		switch event.EndedBy {
		case "snooze":
			snoozed = true
		case "stop":
			stopped = true
		}
	}
	if !snoozed || !stopped {
		t.Fatalf("events = %+v, want one snooze and one stop", events)
	}
	if player.startCount() < 2 {
		t.Fatalf("ring starts = %d, want at least 2", player.startCount())
	}
	list := manager.List()
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	if len(list) != 1 || !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("List = %+v, want next %s", list, wantNext)
	}
	var nextMS int64
	var snoozeMS *int64
	if err := st.DB().QueryRow(`SELECT next_fires_at, snoozed_until FROM alarms`).Scan(&nextMS, &snoozeMS); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if nextMS != wantNext.UnixMilli() || snoozeMS != nil {
		t.Fatalf("row next = %d snooze = %v, want %d and NULL", nextMS, snoozeMS, wantNext.UnixMilli())
	}
}

// TestCancelDuringRingStopsAndRemoves proves a cancel racing a fire ends the
// Ring as stopped/cancel, deletes the row and schedules nothing.
func TestCancelDuringRingStopsAndRemoves(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	player := newRingPlayer(func(_ int, ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	manager := newManager(t, clock, nil, nil, nil)
	manager.SetRingEngine(testEngine(registry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case <-player.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the Alarm never rang")
	}
	cancelled, err := manager.Cancel(context.Background(), alarm.CancelIn{})
	if err != nil || cancelled != 1 {
		t.Fatalf("Cancel = %d, %v; want 1", cancelled, err)
	}
	events := waitAlarmHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "cancel" {
		t.Fatalf("event = %+v, want stopped/cancel", events[0])
	}
	if rows := countAlarmRows(t, st); rows != 0 {
		t.Fatalf("alarm rows = %d after cancel, want 0", rows)
	}
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("List = %+v after cancel, want empty", list)
	}
}

// TestDowntimeRuleFiresRecentAndSummarisesOld covers the shared two-minute
// rule: a recent occurrence Rings now, an older one is missed and repeats next.
func TestDowntimeRuleFiresRecentAndSummarisesOld(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	base := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: base}
	rec := newSayRecorder()
	queueRec := newSayRecorder()
	queue := announce.New(queueRec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer queue.Close()

	// A one-off overdue by one minute, and a daily Alarm overdue by ten.
	if _, err := st.DB().Exec(
		`INSERT INTO alarms (label, hour, minute, schedule, weekdays, next_fires_at, created_at)
		 VALUES ('recent', 7, 59, 'once', 0, ?, ?)`,
		base.Add(-time.Minute).UnixMilli(), base.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("insert recent: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO alarms (label, hour, minute, schedule, weekdays, next_fires_at, created_at)
		 VALUES ('old', 7, 50, 'daily', 0, ?, ?)`,
		base.Add(-10*time.Minute).UnixMilli(), base.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("insert old: %v", err)
	}

	manager := newManager(t, clock, rec.SayText, queue, nil)
	if err := manager.AttachStore(st, queue); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	select {
	case text := <-rec.texts:
		if text != "Recent alarm is going off." {
			t.Fatalf("recent alert = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the recent Alarm did not fire at startup")
	}
	select {
	case text := <-queueRec.texts:
		if text != "You missed an alarm: old at 7:50 am." {
			t.Fatalf("summary = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the downtime summary was not queued")
	}
	events := waitAlarmHistory(t, manager, 2)
	var missed *alertstore.Event
	for i := range events {
		if events[i].Kind == "alarm" && events[i].Label == "old" {
			missed = &events[i]
		}
	}
	if missed == nil || missed.State != alertstore.StateMissed || missed.EndedBy != "downtime" {
		t.Fatalf("old event = %+v, want missed/downtime", missed)
	}
	list := manager.List()
	if len(list) != 1 || list[0].Label != "old" {
		t.Fatalf("List = %+v, want only the repeating old Alarm", list)
	}
	wantNext := time.Date(2026, 11, 13, 7, 50, 0, 0, time.UTC)
	if !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("old next = %s, want %s", list[0].NextFiresAt, wantNext)
	}
}

// TestRecoveryKeepsOneEntryAndSnoozeSurvivesRestart proves the persisted
// schedule is recovered once, and a pending snooze re-rings after a restart.
func TestRecoveryKeepsOneEntryAndSnoozeSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	base := time.Date(2026, 11, 12, 7, 0, 1, 0, time.UTC)
	clock := &fixedClock{now: base}

	firstStore := openStore(t, path)
	first := newManager(t, clock, nil, nil, nil)
	if err := first.AttachStore(firstStore, nil); err != nil {
		t.Fatalf("first AttachStore: %v", err)
	}
	if _, err := first.Set(context.Background(), alarm.SetIn{Hour: 8, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if list := first.List(); len(list) != 1 {
		t.Fatalf("first List = %+v", list)
	}
	first.Close()
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	secondStore := openStore(t, path)
	defer secondStore.Close()
	second := newManager(t, clock, nil, nil, nil)
	if err := second.AttachStore(secondStore, nil); err != nil {
		t.Fatalf("second AttachStore: %v", err)
	}
	list := second.List()
	if len(list) != 1 {
		t.Fatalf("second List = %+v, want one recovered Alarm", list)
	}
	// A pending snooze row is a second durable fire even when next_fires_at is
	// the repeating occurrence.
	if _, err := secondStore.DB().Exec(
		`UPDATE alarms SET snoozed_until = ?`, base.Add(20*time.Millisecond).UnixMilli()); err != nil {
		t.Fatalf("set snooze: %v", err)
	}
	second.Close()
	thirdStore := openStore(t, path)
	defer thirdStore.Close()
	rec := newSayRecorder()
	third := newManager(t, clock, rec.SayText, nil, nil)
	if err := third.AttachStore(thirdStore, nil); err != nil {
		t.Fatalf("third AttachStore: %v", err)
	}
	select {
	case text := <-rec.texts:
		if text != "Your alarm is going off." {
			t.Fatalf("snooze alert = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the recovered snooze did not fire")
	}
}
