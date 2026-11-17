package alarm_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// TestTimezoneChangeReschedulesPendingDailyAlarm verifies that when the timezone
// setting changes, a pending daily clock-time alarm is recomputed in the new
// timezone, updates SQLite next_fires_at, and reschedules the live timer.
func TestTimezoneChangeReschedulesPendingDailyAlarm(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()

	svc, err := settings.New(ctx, st, tz.Definition(""))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	zone.Bind(svc)

	// In July, Europe/London is in BST (UTC+1). A 7:00 local alarm is 06:00:00 UTC.
	// Setting clock now to 100ms before 06:00:00 UTC means:
	// - in UTC (7:00 UTC), delay is ~1 hour 100ms.
	// - in Europe/London (7:00 BST = 6:00 UTC), delay is 100ms.
	clockNow := time.Date(2026, 7, 15, 5, 59, 59, 900_000_000, time.UTC)
	clock := &fixedClock{now: clockNow}

	focusRegistry := focus.New()
	player := newRingPlayer(func(_ int, ringCtx context.Context) error {
		<-ringCtx.Done()
		return ringCtx.Err()
	})
	engine := testEngine(focusRegistry, player)

	manager := alarm.NewManager(alarm.Options{
		Now:      clock.Now,
		Zone:     zone,
		Settings: func() *settings.Service { return svc },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	manager.SetRingEngine(engine)

	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	// 1. Set a daily 7:00 alarm while in UTC.
	created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "morning"})
	if err != nil {
		t.Fatalf("Set UTC daily alarm: %v", err)
	}
	utcExpected := time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC)
	if !created.NextFiresAt.Equal(utcExpected) {
		t.Fatalf("created.NextFiresAt = %s, want %s", created.NextFiresAt, utcExpected)
	}

	// Verify database row has next_fires_at in UTC.
	var nextMS int64
	if err := st.DB().QueryRow(`SELECT next_fires_at FROM alarms WHERE id = ?`, created.ID).Scan(&nextMS); err != nil {
		t.Fatalf("query next_fires_at: %v", err)
	}
	if nextMS != utcExpected.UnixMilli() {
		t.Fatalf("db next_fires_at = %d, want %d", nextMS, utcExpected.UnixMilli())
	}

	// 2. Switch timezone setting to Europe/London.
	if _, err := svc.Set(ctx, tz.Key, "Europe/London"); err != nil {
		t.Fatalf("switch timezone setting: %v", err)
	}

	// 3. Assert pending instant moves by offset (7:00 BST = 06:00:00 UTC).
	londonExpected := time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC)
	list := manager.List()
	if len(list) != 1 {
		t.Fatalf("List returned %d alarms, want 1", len(list))
	}
	if !list[0].NextFiresAt.Equal(londonExpected) {
		t.Fatalf("after tz switch NextFiresAt = %s, want %s", list[0].NextFiresAt, londonExpected)
	}

	// Assert SQLite was updated.
	if err := st.DB().QueryRow(`SELECT next_fires_at FROM alarms WHERE id = ?`, created.ID).Scan(&nextMS); err != nil {
		t.Fatalf("query next_fires_at after switch: %v", err)
	}
	if nextMS != londonExpected.UnixMilli() {
		t.Fatalf("db next_fires_at after switch = %d, want %d", nextMS, londonExpected.UnixMilli())
	}

	// 4. Assert live timer was rescheduled: delay is now 100ms so it rings promptly.
	select {
	case <-player.started:
		// Ring started, timer fired at the rescheduled 06:00:00 UTC instant!
	case <-time.After(3 * time.Second):
		t.Fatal("rescheduled timer never fired")
	}
}

// TestTimezoneChangeKeepsSnoozedAlarmInstant verifies that a pending snooze
// is treated as an absolute instant and is not recomputed when the timezone changes.
func TestTimezoneChangeKeepsSnoozedAlarmInstant(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()

	svc, err := settings.New(ctx, st, tz.Definition(""))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	zone.Bind(svc)

	clockNow := time.Date(2026, 7, 15, 6, 59, 59, 900_000_000, time.UTC)
	clock := &fixedClock{now: clockNow}

	focusRegistry := focus.New()
	player := newRingPlayer(func(_ int, ringCtx context.Context) error {
		<-ringCtx.Done()
		return ringCtx.Err()
	})
	engine := testEngine(focusRegistry, player)

	manager := alarm.NewManager(alarm.Options{
		Now:      clock.Now,
		Zone:     zone,
		Settings: func() *settings.Service { return svc },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	manager.SetRingEngine(engine)

	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "wake"})
	if err != nil {
		t.Fatalf("Set alarm: %v", err)
	}

	// Wait for the alarm to ring.
	select {
	case <-player.started:
	case <-time.After(3 * time.Second):
		t.Fatal("alarm did not ring")
	}

	// Snooze the ringing alarm for 10 minutes.
	snoozeTool := alarm.NewSnooze(manager)
	out, err := snoozeTool.Handler(ctx, alarm.SnoozeIn{DurationSeconds: 600})
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	if !out.Snoozed {
		t.Fatalf("SnoozeOut = %+v, want Snoozed=true", out)
	}

	// Wait for the asynchronous finishRing to persist the snoozed_until row.
	wantSnooze := clockNow.Add(600 * time.Second).UnixMilli()
	waitSnoozedUntil(t, st, wantSnooze)

	// Snooze instant should be clockNow + 600s = 07:09:59.900 UTC.
	listBefore := manager.List()
	if len(listBefore) != 1 {
		t.Fatalf("List before tz change = %+v, want 1 alarm", listBefore)
	}
	snoozeInstant := listBefore[0].NextFiresAt

	// Switch timezone to Europe/London.
	if _, err := svc.Set(ctx, tz.Key, "Europe/London"); err != nil {
		t.Fatalf("switch timezone setting: %v", err)
	}

	// Assert the snoozed alarm keeps its snooze instant.
	listAfter := manager.List()
	if len(listAfter) != 1 {
		t.Fatalf("List after tz change = %+v, want 1 alarm", listAfter)
	}
	if !listAfter[0].NextFiresAt.Equal(snoozeInstant) {
		t.Fatalf("snoozed NextFiresAt after tz change = %s, want original snooze instant %s",
			listAfter[0].NextFiresAt, snoozeInstant)
	}

	// Assert SQLite snoozed_until remains unchanged.
	var snoozeMSAfter int64
	if err := st.DB().QueryRow(`SELECT snoozed_until FROM alarms WHERE id = ?`, created.ID).Scan(&snoozeMSAfter); err != nil {
		t.Fatalf("query snoozed_until after tz change: %v", err)
	}
	if snoozeMSAfter != snoozeInstant.UnixMilli() {
		t.Fatalf("db snoozed_until after tz change = %d, want %d", snoozeMSAfter, snoozeInstant.UnixMilli())
	}
}

// TestTimezoneChangeReschedulesPendingOnceAlarm verifies that a one-off (Once)
// alarm is also rescheduled when the timezone changes.
func TestTimezoneChangeReschedulesPendingOnceAlarm(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()

	svc, err := settings.New(ctx, st, tz.Definition(""))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	zone.Bind(svc)

	clockNow := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: clockNow}

	manager := alarm.NewManager(alarm.Options{
		Now:      clock.Now,
		Zone:     zone,
		Settings: func() *settings.Service { return svc },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)

	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "once", Label: "flight"})
	if err != nil {
		t.Fatalf("Set once alarm: %v", err)
	}
	utcExpected := time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC)
	if !created.NextFiresAt.Equal(utcExpected) {
		t.Fatalf("created.NextFiresAt = %s, want %s", created.NextFiresAt, utcExpected)
	}

	// Switch timezone to Europe/London (UTC+1 in July).
	if _, err := svc.Set(ctx, tz.Key, "Europe/London"); err != nil {
		t.Fatalf("switch timezone setting: %v", err)
	}

	londonExpected := time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC)
	list := manager.List()
	if len(list) != 1 || !list[0].NextFiresAt.Equal(londonExpected) {
		t.Fatalf("once alarm NextFiresAt = %s, want %s", list[0].NextFiresAt, londonExpected)
	}
}
