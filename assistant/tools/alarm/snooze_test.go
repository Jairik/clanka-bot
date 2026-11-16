package alarm_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// waitSnoozedUntil waits for the snooze row the finishing Ring writes, then
// checks it holds the requested re-ring instant.
func waitSnoozedUntil(t *testing.T, st *store.Store, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got *int64
		if err := st.DB().QueryRow(`SELECT snoozed_until FROM alarms`).Scan(&got); err != nil {
			t.Fatalf("read snooze: %v", err)
		}
		if got != nil {
			if *got != want {
				t.Fatalf("snoozed_until = %d, want %d", *got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("snoozed_until was never set, want %d", want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSnoozeToolDefaultDuration proves a phrased snooze with no spoken delay
// uses the live ring.alarm.snooze setting, writes the same ended_by=snooze
// history the Focus path writes, and persists the re-ring time.
func TestSnoozeToolDefaultDuration(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	outs := make(chan alarm.SnoozeOut, 1)
	var manager *alarm.Manager
	player := newRingPlayer(func(pass int, pctx context.Context) error {
		if pass == 1 {
			out, err := manager.Snooze(ctx, alarm.SnoozeIn{})
			if err != nil {
				t.Errorf("Snooze: %v", err)
			}
			outs <- out
		} else {
			registry.HandleIntent(ctx, focus.Stop)
		}
		<-pctx.Done()
		return pctx.Err()
	})
	manager = alarm.NewManager(alarm.Options{
		Now:    clock.Now,
		Snooze: func() time.Duration { return time.Second },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	manager.SetRingEngine(testEngine(registry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	out := <-outs
	if !out.Snoozed || out.DurationSeconds != 1 {
		t.Fatalf("SnoozeOut = %+v, want snoozed with a 1s default", out)
	}
	events := waitAlarmHistory(t, manager, 1)
	var sawSnooze bool
	for _, event := range events {
		if event.Kind == "alarm" && event.EndedBy == "snooze" {
			sawSnooze = true
		}
	}
	if !sawSnooze {
		t.Fatalf("history = %+v, want an ended_by=snooze row", events)
	}
	want := clock.Now().Add(time.Second).UnixMilli()
	waitSnoozedUntil(t, st, want)
	if text := alarm.NewSnooze(manager).Speak(out); text != "Alarm snoozed for 1 second." {
		t.Fatalf("Speak = %q, want the default confirmation", text)
	}
}

// TestSnoozeToolExplicitDurationReringsAndKeepsRepeat is the spoken-duration
// acceptance path: the explicit delay overrides the live default, the Alarm
// rings again at that instant, and its repeat schedule is untouched.
func TestSnoozeToolExplicitDurationReringsAndKeepsRepeat(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	registry := focus.New()
	outs := make(chan alarm.SnoozeOut, 1)
	var manager *alarm.Manager
	player := newRingPlayer(func(pass int, pctx context.Context) error {
		if pass == 1 {
			out, err := manager.Snooze(ctx, alarm.SnoozeIn{DurationSeconds: 1})
			if err != nil {
				t.Errorf("Snooze: %v", err)
			}
			outs <- out
		} else {
			if text, handled := registry.HandleIntent(ctx, focus.Stop); !handled || text != "Alarm stopped." {
				t.Errorf("stop = %q, %v; want Alarm stopped", text, handled)
			}
		}
		<-pctx.Done()
		return pctx.Err()
	})
	// The 50 ms seam must lose to the explicit one-second delay.
	manager = alarm.NewManager(alarm.Options{
		Now:    clock.Now,
		Snooze: func() time.Duration { return 50 * time.Millisecond },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	manager.SetRingEngine(testEngine(registry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	out := <-outs
	if !out.Snoozed || out.DurationSeconds != 1 {
		t.Fatalf("SnoozeOut = %+v, want snoozed for 1s", out)
	}
	want := clock.Now().Add(time.Second).UnixMilli()
	waitSnoozedUntil(t, st, want) // explicit 1s, not the 50ms default

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
		t.Fatalf("history = %+v, want one snooze and one stop", events)
	}
	if got := player.startCount(); got < 2 {
		t.Fatalf("ring starts = %d, want the Alarm to ring again", got)
	}
	list := manager.List()
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	if len(list) != 1 || !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("List = %+v, want the repeat at %s", list, wantNext)
	}
	var nextMS int64
	if err := st.DB().QueryRow(`SELECT next_fires_at FROM alarms`).Scan(&nextMS); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if nextMS != wantNext.UnixMilli() {
		t.Fatalf("next_fires_at = %d, want %d", nextMS, wantNext.UnixMilli())
	}
}

// TestSnoozeToolWithoutRingingAlarmRefuses proves a snooze with nothing
// ringing answers clearly instead of silently doing nothing.
func TestSnoozeToolWithoutRingingAlarmRefuses(t *testing.T) {
	manager := alarm.NewManager(alarm.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer manager.Close()

	out, err := manager.Snooze(context.Background(), alarm.SnoozeIn{DurationSeconds: 600})
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	if out.Snoozed {
		t.Fatalf("SnoozeOut = %+v, want Snoozed=false", out)
	}
	tool := alarm.NewSnooze(manager)
	if text := tool.Speak(out); text != "There's no alarm ringing to snooze." {
		t.Fatalf("Speak = %q, want a clear refusal", text)
	}
}

// TestSnoozeToolExtract reads the phrased form with the shared duration
// parser, and leaves the bare control phrase and bare durations alone.
func TestSnoozeToolExtract(t *testing.T) {
	manager := alarm.NewManager(alarm.Options{})
	defer manager.Close()
	tool := alarm.NewSnooze(manager)

	cases := []struct {
		text    string
		ok      bool
		seconds int
	}{
		{"snooze the alarm", true, 0},
		{"snooze my alarm", true, 0},
		{"snooze the alarm for 10 minutes", true, 600},
		{"snooze the alarm for twenty minutes", true, 1200},
		{"snooze for five more minutes", true, 300},
		{"snooze the wake up alarm for 15 minutes", true, 900},
		{"snooze", false, 0}, // bare Control phrase, handled by Focus
		{"set an alarm for 7", false, 0},
		{"give me ten more minutes", false, 0}, // no snooze word; left to timer/Agent
	}
	for _, tc := range cases {
		in, ok := tool.Extract(tc.text)
		if ok != tc.ok {
			t.Errorf("Extract(%q) = %v, want %v", tc.text, ok, tc.ok)
			continue
		}
		if ok && in.DurationSeconds != tc.seconds {
			t.Errorf("Extract(%q).DurationSeconds = %d, want %d", tc.text, in.DurationSeconds, tc.seconds)
		}
	}
	if text := tool.Speak(alarm.SnoozeOut{Snoozed: true, DurationSeconds: 600}); text != "Alarm snoozed for 10 minutes." {
		t.Fatalf("Speak = %q, want the spoken-duration confirmation", text)
	}
}
