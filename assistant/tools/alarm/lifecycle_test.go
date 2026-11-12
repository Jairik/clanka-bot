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
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// TestRingTimeoutRecordsMissedAndContinuesTheRepeat covers max_ring: the Alarm
// is missed, the hook reports it, and a daily Alarm still schedules tomorrow.
func TestRingTimeoutRecordsMissedAndContinuesTheRepeat(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	player := newRingPlayer(nil)
	engine := ring.New(ring.Deps{
		Focus: registry,
		Settings: func(ring.Kind) ring.Settings {
			return ring.Settings{MaxRing: 5 * time.Millisecond, Volume: 100, Gap: time.Millisecond}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) {
			return sound.Clip{
				PCM:    []byte{0, 0, 0, 0},
				Format: audio.Format{Rate: 8000, Width: 2, Channels: 1},
			}, true
		},
		Play:  player.Play,
		Clock: &fastClock{now: time.Now()},
	})
	manager := newManager(t, clock, nil, nil, nil)
	manager.SetRingEngine(engine)
	missed := make(chan alarm.MissedAlert, 1)
	manager.SetMissedHook(func(alert alarm.MissedAlert) { missed <- alert })
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily", Label: "wake"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case alert := <-missed:
		if alert.Reason != "max_ring" || alert.Label != "wake" {
			t.Fatalf("missed = %+v", alert)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the missed hook did not run at max_ring")
	}
	events := waitAlarmHistory(t, manager, 1)
	if events[0].State != alertstore.StateMissed || events[0].EndedBy != "timeout" {
		t.Fatalf("event = %+v, want missed/timeout", events[0])
	}
	list := manager.List()
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	if len(list) != 1 || !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("List = %+v, want next %s", list, wantNext)
	}
}

// TestTimezoneIsReadPerSchedule proves the manager reads the live IANA zone on
// every scheduling call instead of snapshotting it at registration.
func TestTimezoneIsReadPerSchedule(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	svc, err := settings.New(ctx, st, tz.Definition(""))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	zone.Bind(svc)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	manager := alarm.NewManager(alarm.Options{
		Now:    clock.Now,
		Zone:   zone,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	utc, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily"})
	if err != nil {
		t.Fatalf("Set UTC: %v", err)
	}
	if want := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC); !utc.NextFiresAt.Equal(want) {
		t.Fatalf("UTC next = %s, want %s", utc.NextFiresAt, want)
	}
	if _, err := svc.SetJSON(ctx, tz.Key, []byte(`"Europe/Berlin"`)); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	berlin, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily"})
	if err != nil {
		t.Fatalf("Set Berlin: %v", err)
	}
	if want := time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC); !berlin.NextFiresAt.Equal(want) {
		t.Fatalf("Berlin next = %s, want %s", berlin.NextFiresAt, want)
	}
}

// TestListAndCancelSelectors covers time, label, id and all selectors.
func TestListAndCancelSelectors(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newManager(t, clock, nil, nil, nil)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	work, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("Set work: %v", err)
	}
	_ = work
	gym, err := manager.Set(context.Background(), alarm.SetIn{Hour: 8, Minute: 30, Label: "gym"})
	if err != nil {
		t.Fatalf("Set gym: %v", err)
	}
	list := manager.List()
	if len(list) != 2 || list[0].Label != "work" || list[1].Label != "gym" {
		t.Fatalf("List = %+v, want work then gym", list)
	}

	if cancelled, err := manager.Cancel(context.Background(), alarm.CancelIn{Time: "7:00"}); err != nil || cancelled != 1 {
		t.Fatalf("cancel 7:00 = %d, %v; want 1", cancelled, err)
	}
	if list := manager.List(); len(list) != 1 || list[0].ID != gym.ID {
		t.Fatalf("after time cancel = %+v", list)
	}
	if cancelled, err := manager.Cancel(context.Background(), alarm.CancelIn{Label: "gym"}); err != nil || cancelled != 1 {
		t.Fatalf("cancel gym = %d, %v; want 1", cancelled, err)
	}
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("after label cancel = %+v", list)
	}

	first, _ := manager.Set(context.Background(), alarm.SetIn{Hour: 9})
	second, _ := manager.Set(context.Background(), alarm.SetIn{Hour: 10})
	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 11}); err != nil {
		t.Fatalf("Set 11: %v", err)
	}
	if cancelled, err := manager.Cancel(context.Background(), alarm.CancelIn{ID: first.ID}); err != nil || cancelled != 1 {
		t.Fatalf("cancel id = %d, %v; want 1", cancelled, err)
	}
	if cancelled := manager.CancelAll(); cancelled != 2 {
		t.Fatalf("CancelAll = %d, want 2 (second %s)", cancelled, second.ID)
	}
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("after CancelAll = %+v", list)
	}
}

// TestToolsExtractSetListAndCancel is the Fast path journey: real Extract,
// Execute, Speak and Local-only metadata.
func TestToolsExtractSetListAndCancel(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newManager(t, clock, nil, nil, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	info, ok := reg.Info(alarm.ListName)
	if !ok || !info.LocalOnly || info.Permission != tool.PermissionRead {
		t.Fatalf("alarm.list info = %+v, want a local-only read", info)
	}
	for _, name := range []string{alarm.SetName, alarm.CancelName} {
		info, ok := reg.Info(name)
		if !ok || info.LocalOnly {
			t.Fatalf("%s info = %+v, want LocalOnlyNo", name, info)
		}
	}

	set := func(utterance string) alarm.SetOut {
		t.Helper()
		args, ok := reg.Extract(alarm.SetName, utterance)
		if !ok {
			t.Fatalf("Extract(%q) failed", utterance)
		}
		result, err := reg.Execute(context.Background(), alarm.SetName, args)
		if err != nil {
			t.Fatalf("Execute(%q): %v", utterance, err)
		}
		return result.(alarm.SetOut)
	}
	wake := set("wake me at 7")
	if wake.Hour != 7 || wake.Schedule != alarm.Once {
		t.Fatalf("wake = %+v", wake)
	}
	if want := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC); !wake.NextFiresAt.Equal(want) {
		t.Fatalf("wake next = %s, want %s", wake.NextFiresAt, want)
	}
	weekdays := set("set an alarm for 6:30 on weekdays")
	if weekdays.Schedule != alarm.Weekdays || weekdays.Hour != 6 || weekdays.Minute != 30 {
		t.Fatalf("weekdays = %+v", weekdays)
	}
	chosen := set("set an alarm for 6:45 on monday and wednesday")
	if chosen.Schedule != alarm.Chosen || len(chosen.Weekdays) != 2 || chosen.Weekdays[0] != "monday" || chosen.Weekdays[1] != "wednesday" {
		t.Fatalf("chosen = %+v", chosen)
	}

	result, err := reg.Execute(context.Background(), alarm.ListName, nil)
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	if list := result.(alarm.ListOut); len(list.Alarms) != 3 {
		t.Fatalf("list = %+v, want 3", list)
	}
	if text, ok := reg.Speak(alarm.ListName, result); !ok || text == "" {
		t.Fatal("alarm.list has no Speak line")
	}

	args, ok := reg.Extract(alarm.CancelName, "cancel my 7 am alarm")
	if !ok {
		t.Fatal("cancel Extract failed")
	}
	result, err = reg.Execute(context.Background(), alarm.CancelName, args)
	if err != nil {
		t.Fatalf("Execute cancel: %v", err)
	}
	if out := result.(alarm.CancelOut); out.Cancelled != 1 {
		t.Fatalf("cancel = %+v, want 1", out)
	}
	args, _ = reg.Extract(alarm.CancelName, "cancel my alarm")
	result, err = reg.Execute(context.Background(), alarm.CancelName, args)
	if err != nil {
		t.Fatalf("Execute cancel all: %v", err)
	}
	if out := result.(alarm.CancelOut); out.Cancelled != 2 {
		t.Fatalf("cancel all = %+v, want 2", out)
	}

	// Extraction refuses a request that is not about an alarm.
	if _, ok := reg.Extract(alarm.SetName, "set a timer for 10 minutes"); ok {
		t.Error("a timer request extracted as alarm.set")
	}
	if _, ok := reg.Extract(alarm.CancelName, "cancel my subscription"); ok {
		t.Error("a subscription cancel extracted as alarm.cancel")
	}
}

// TestSnoozeUsesTheLiveRingSetting proves the scheduler reads
// ring.alarm.snooze per snooze action rather than hardcoding 9 minutes.
func TestSnoozeUsesTheLiveRingSetting(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	svc, err := settings.New(ctx, st, ring.Definitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if _, err := svc.SetJSON(ctx, ring.Key(ring.Alarm, "snooze"), []byte(`"1s"`)); err != nil {
		t.Fatalf("set snooze: %v", err)
	}
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	player := newRingPlayer(func(_ int, ctx context.Context) error {
		registry.HandleIntent(context.Background(), focus.Snooze)
		<-ctx.Done()
		return ctx.Err()
	})
	manager := alarm.NewManager(alarm.Options{
		Now:      clock.Now,
		Settings: func() *settings.Service { return svc },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	manager.SetRingEngine(testEngine(registry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	waitAlarmHistory(t, manager, 1)

	want := clock.Now().Add(time.Second).UnixMilli()
	deadline := time.Now().Add(time.Second)
	for {
		var snoozeMS *int64
		if err := st.DB().QueryRow(`SELECT snoozed_until FROM alarms`).Scan(&snoozeMS); err != nil {
			t.Fatalf("read snooze: %v", err)
		}
		if snoozeMS != nil {
			if *snoozeMS != want {
				t.Fatalf("snoozed_until = %d, want %d from the live 1s setting", *snoozeMS, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the snooze was never persisted")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestConcurrentSetListCancel is the race-detector surface for the manager.
func TestConcurrentSetListCancel(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newManager(t, clock, nil, nil, nil)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(hour int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = manager.Set(context.Background(), alarm.SetIn{Hour: hour, Schedule: "daily"})
				_ = manager.List()
				_, _ = manager.Cancel(context.Background(), alarm.CancelIn{})
			}
		}(7 + i%4)
	}
	wg.Wait()
}
