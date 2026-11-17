package reminder_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// TestReminderTimezoneReschedule verifies that pending clock-time reminders
// are rescheduled when the timezone changes, while duration reminders keep
// their absolute target instant.
func TestReminderTimezoneReschedule(t *testing.T) {
	ctx := context.Background()
	st := openReminderStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()

	svc, err := settings.New(ctx, st, tz.Definition(""))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	zone := tz.New("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	zone.Bind(svc)

	fixedNow := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	manager := reminder.NewManager(reminder.Options{
		Now:      func() time.Time { return fixedNow },
		Zone:     zone,
		Settings: func() *settings.Service { return svc },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)

	if err := manager.AttachStore(st); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	// 1. Schedule a clock reminder in UTC for 17:00 (5:00 pm).
	clockReminder, err := manager.StartClock("call mum", 17, 0, false)
	if err != nil {
		t.Fatalf("StartClock: %v", err)
	}
	utcExpected := time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC)
	if !clockReminder.FiresAt.Equal(utcExpected) {
		t.Fatalf("clockReminder.FiresAt = %s, want %s", clockReminder.FiresAt, utcExpected)
	}

	// 2. Schedule a duration reminder for 20 minutes out.
	durReminder, err := manager.StartDuration("take bins out", 20*time.Minute)
	if err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	durExpected := fixedNow.Add(20 * time.Minute)
	if !durReminder.FiresAt.Equal(durExpected) {
		t.Fatalf("durReminder.FiresAt = %s, want %s", durReminder.FiresAt, durExpected)
	}

	// 3. Switch timezone setting to Europe/London (BST, UTC+1 in July).
	if _, err := svc.Set(ctx, tz.Key, "Europe/London"); err != nil {
		t.Fatalf("switch timezone setting: %v", err)
	}

	// 4. Assert clock reminder moved by the 1-hour offset (17:00 BST = 16:00 UTC).
	londonExpected := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	list := manager.List()
	var foundClock, foundDuration *reminder.Reminder
	for i := range list {
		if list[i].ID == clockReminder.ID {
			foundClock = &list[i]
		}
		if list[i].ID == durReminder.ID {
			foundDuration = &list[i]
		}
	}
	if foundClock == nil {
		t.Fatalf("clock reminder %s missing from List", clockReminder.ID)
	}
	if !foundClock.FiresAt.Equal(londonExpected) {
		t.Fatalf("after tz switch clock FiresAt = %s, want %s", foundClock.FiresAt, londonExpected)
	}

	// Assert SQLite fires_at for clock reminder was updated.
	var firesMS int64
	if err := st.DB().QueryRow(`SELECT fires_at FROM reminders WHERE id = ?`, clockReminder.ID).Scan(&firesMS); err != nil {
		t.Fatalf("query fires_at: %v", err)
	}
	if firesMS != londonExpected.UnixMilli() {
		t.Fatalf("db fires_at after tz switch = %d, want %d", firesMS, londonExpected.UnixMilli())
	}

	// 5. Assert duration reminder stayed at its absolute instant.
	if foundDuration == nil {
		t.Fatalf("duration reminder %s missing from List", durReminder.ID)
	}
	if !foundDuration.FiresAt.Equal(durExpected) {
		t.Fatalf("duration reminder FiresAt moved to %s, want original %s", foundDuration.FiresAt, durExpected)
	}
}
