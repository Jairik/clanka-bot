package alarm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// extractUpdateIn runs the Fast-path Extract for alarm.update and decodes the
// typed request, so the tests exercise the same parse the voice path uses.
func extractUpdateIn(t *testing.T, reg *tool.Registry, utterance string) alarm.UpdateIn {
	t.Helper()
	raw, ok := reg.Extract(alarm.UpdateName, utterance)
	if !ok {
		t.Fatalf("Extract(%q) did not match alarm.update", utterance)
	}
	var in alarm.UpdateIn
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("Unmarshal(%q): %v", utterance, err)
	}
	return in
}

// newRenameManager is an in-memory Manager with a fake clock, for checking the
// live callback after a retime without a store.
func newRenameManager(t *testing.T, clock *fixedClock) *alarm.Manager {
	t.Helper()
	manager := alarm.NewManager(alarm.Options{
		Now:    clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(manager.Close)
	return manager
}

// TestRenameInPlaceKeepsIDAndSchedule proves a spoken rename writes only the
// label: the ID, schedule, overrides and next occurrence are untouched, and no
// second row is created.
func TestRenameInPlaceKeepsIDAndSchedule(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	created, err := manager.Set(ctx, alarm.SetIn{
		Hour: 7, Schedule: "weekdays", Label: "work",
		Sound: "birds", Volume: intPtr(35), RampSeconds: intPtr(10),
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	var beforeNext int64
	if err := fixture.store.DB().QueryRow(`SELECT next_fires_at FROM alarms WHERE id = ?`, created.ID).
		Scan(&beforeNext); err != nil {
		t.Fatalf("read next before: %v", err)
	}

	in := extractUpdateIn(t, reg, "rename my work alarm to gym")
	if in.Label != "work" || in.NewLabel == nil || *in.NewLabel != "gym" {
		t.Fatalf("Extract rename = %+v; want selector work, new label gym", in)
	}
	out, err := manager.Update(ctx, in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Matched != 1 || out.Alarm.ID != created.ID || out.Alarm.Label != "gym" {
		t.Fatalf("rename result = %+v", out)
	}
	if out.Alarm.Schedule != alarm.Weekdays || !out.Alarm.NextFiresAt.Equal(created.NextFiresAt) {
		t.Fatalf("rename changed schedule/occurrence: %+v", out.Alarm)
	}
	if out.Alarm.Sound != "birds" || out.Alarm.Volume == nil || *out.Alarm.Volume != 35 ||
		out.Alarm.RampSeconds == nil || *out.Alarm.RampSeconds != 10 {
		t.Fatalf("rename changed overrides: %+v", out.Alarm)
	}

	var (
		label, schedule, sound string
		hour, minute           int
		afterNext              int64
		count                  int
	)
	if err := fixture.store.DB().QueryRow(
		`SELECT label, hour, minute, schedule, sound, next_fires_at FROM alarms WHERE id = ?`, created.ID).
		Scan(&label, &hour, &minute, &schedule, &sound, &afterNext); err != nil {
		t.Fatalf("read row after: %v", err)
	}
	if err := fixture.store.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if label != "gym" || hour != 7 || minute != 0 || schedule != "weekdays" || sound != "birds" || afterNext != beforeNext {
		t.Fatalf("stored row after rename = %q %d:%d %s %s %d", label, hour, minute, schedule, sound, afterNext)
	}
	if count != 1 {
		t.Fatalf("alarm rows = %d, want 1 (no duplicate)", count)
	}

	text, ok := reg.Speak(alarm.UpdateName, out)
	if !ok || text != "Renamed your 7:00 am alarm to gym." {
		t.Fatalf("Speak rename = %q, ok=%v", text, ok)
	}
}

// TestRetimeMovesNextFireForOnceAndRepeating checks that a spoken move
// recomputes the occurrence from the fake clock for both schedule kinds, stores
// the new clock in place and names the labelled Alarm in the reply.
func TestRetimeMovesNextFireForOnceAndRepeating(t *testing.T) {
	cases := []struct {
		name     string
		utter    string
		schedule string
		wantNext time.Time
		wantHour int
		wantMin  int
	}{
		{
			name: "once", utter: "move my gym alarm to 6:30", schedule: "once",
			wantNext: time.Date(2026, 11, 12, 6, 30, 0, 0, time.UTC), wantHour: 6, wantMin: 30,
		},
		{
			name: "daily", utter: "move my gym alarm to 8", schedule: "daily",
			wantNext: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC), wantHour: 8, wantMin: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newOverrideFixture(t)
			clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
			manager := newOverrideManager(t, clock, fixture, nil)
			reg := tool.NewRegistry(5 * time.Second)
			if err := alarm.Register(reg, manager); err != nil {
				t.Fatalf("Register: %v", err)
			}
			created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: tc.schedule, Label: "gym"})
			if err != nil {
				t.Fatalf("Set: %v", err)
			}

			in := extractUpdateIn(t, reg, tc.utter)
			if in.Label != "gym" || in.NewTime == "" {
				t.Fatalf("Extract(%q) = %+v; want selector gym and a new time", tc.utter, in)
			}
			out, err := manager.Update(ctx, in)
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if out.Matched != 1 || out.Alarm.ID != created.ID ||
				out.Alarm.Hour != tc.wantHour || out.Alarm.Minute != tc.wantMin ||
				!out.Alarm.NextFiresAt.Equal(tc.wantNext) {
				t.Fatalf("retime result = %+v; want %s at %s", out, tc.utter, tc.wantNext)
			}
			if out.Alarm.Schedule != created.Schedule {
				t.Fatalf("retime changed schedule: %s -> %s", created.Schedule, out.Alarm.Schedule)
			}

			var (
				hour, minute int
				nextMS       int64
				count        int
			)
			if err := fixture.store.DB().QueryRow(
				`SELECT hour, minute, next_fires_at FROM alarms WHERE id = ?`, created.ID).
				Scan(&hour, &minute, &nextMS); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if err := fixture.store.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&count); err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if hour != tc.wantHour || minute != tc.wantMin || nextMS != tc.wantNext.UnixMilli() || count != 1 {
				t.Fatalf("stored row = %d:%d %d, count %d", hour, minute, nextMS, count)
			}

			text, ok := reg.Speak(alarm.UpdateName, out)
			wantText := "Gym alarm moved to " + formatWantClock(tc.wantHour, tc.wantMin) + "."
			if !ok || text != wantText {
				t.Fatalf("Speak retime = %q, ok=%v; want %q", text, ok, wantText)
			}
		})
	}
}

// formatWantClock mirrors the spoken 12-hour clock for the test table.
func formatWantClock(hour, minute int) string {
	suffix := "am"
	if hour >= 12 {
		suffix = "pm"
	}
	display := hour
	if display > 12 {
		display -= 12
	}
	if display == 0 {
		display = 12
	}
	return fmt.Sprintf("%d:%02d %s", display, minute, suffix)
}

// TestRetimeReplacesLiveCallback checks the in-memory path: List reports the
// new occurrence and still shows exactly one Alarm.
func TestRetimeReplacesLiveCallback(t *testing.T) {
	ctx := context.Background()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newRenameManager(t, clock)
	if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "gym"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, err := manager.Update(ctx, alarm.UpdateIn{NewTime: "06:30"})
	if err != nil || out.Matched != 1 {
		t.Fatalf("Update = %+v, %v", out, err)
	}
	listed := manager.List()
	if len(listed) != 1 || listed[0].Hour != 6 || listed[0].Minute != 30 ||
		!listed[0].NextFiresAt.Equal(time.Date(2026, 11, 12, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("live alarms after retime = %+v", listed)
	}
}

// TestUnknownLabelRefusesWithoutDuplicate proves an unmatched selector writes
// nothing and answers clearly, rather than creating a second Alarm.
func TestUnknownLabelRefusesWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	created, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily", Label: "work"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	in := extractUpdateIn(t, reg, "rename my missing alarm to gym")
	if in.Label != "missing" || in.NewLabel == nil || *in.NewLabel != "gym" {
		t.Fatalf("Extract unknown label = %+v", in)
	}
	out, err := manager.Update(ctx, in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Matched != 0 || out.Ambiguous {
		t.Fatalf("unknown label result = %+v; want no match", out)
	}
	var label string
	var count int
	if err := fixture.store.DB().QueryRow(`SELECT label FROM alarms WHERE id = ?`, created.ID).Scan(&label); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if err := fixture.store.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if label != "work" || count != 1 {
		t.Fatalf("unknown label wrote label %q, rows %d", label, count)
	}
	text, ok := reg.Speak(alarm.UpdateName, out)
	if !ok || text != "No alarm matched." {
		t.Fatalf("Speak unknown label = %q, ok=%v", text, ok)
	}
}

// TestAmbiguousRenameWritesNothing keeps the existing selector rule: two
// matches mean no write and a question, never an arbitrary edit or a new row.
func TestAmbiguousRenameWritesNothing(t *testing.T) {
	ctx := context.Background()
	fixture := newOverrideFixture(t)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newOverrideManager(t, clock, fixture, nil)
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, hour := range []int{7, 8} {
		if _, err := manager.Set(ctx, alarm.SetIn{Hour: hour, Schedule: "daily", Label: "work"}); err != nil {
			t.Fatalf("Set %d: %v", hour, err)
		}
	}
	in := extractUpdateIn(t, reg, "rename my work alarm to gym")
	out, err := manager.Update(ctx, in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !out.Ambiguous || out.Matched != 2 {
		t.Fatalf("ambiguous result = %+v", out)
	}
	var labels string
	var count int
	if err := fixture.store.DB().QueryRow(`SELECT GROUP_CONCAT(label) FROM alarms`).Scan(&labels); err != nil {
		t.Fatalf("read labels: %v", err)
	}
	if err := fixture.store.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if labels != "work,work" || count != 2 {
		t.Fatalf("ambiguous rename wrote %q with %d rows", labels, count)
	}
	text, ok := reg.Speak(alarm.UpdateName, out)
	if !ok || text != "More than one alarm matches. Which one should I change?" {
		t.Fatalf("Speak ambiguous = %q, ok=%v", text, ok)
	}
}

// TestUpdateExtractKeepsOverridePhrasing guards the existing paths: a rename
// value is not a sound, and a numeric volume is not a new clock time.
func TestUpdateExtractKeepsOverridePhrasing(t *testing.T) {
	manager := alarm.NewManager(alarm.Options{Now: time.Now})
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sound := extractUpdateIn(t, reg, "change my 7 am alarm to birds")
	if sound.Sound != "birds" || sound.NewLabel != nil || sound.NewTime != "" || sound.Time != "07:00" {
		t.Fatalf("sound request = %+v", sound)
	}
	volume := extractUpdateIn(t, reg, "update my alarm volume to 10")
	if volume.Volume == nil || *volume.Volume != 10 || volume.NewTime != "" {
		t.Fatalf("volume request = %+v", volume)
	}
	call := extractUpdateIn(t, reg, "call my 7 am alarm work")
	if call.Time != "07:00" || call.NewLabel == nil || *call.NewLabel != "work" {
		t.Fatalf("call request = %+v", call)
	}
	for _, utterance := range []string{"retitle the alarm as gym", "relabel the alarm as gym"} {
		in := extractUpdateIn(t, reg, utterance)
		if in.NewLabel == nil || *in.NewLabel != "gym" || in.NewTime != "" || in.Sound != "" {
			t.Fatalf("Extract(%q) = %+v; want a rename to gym", utterance, in)
		}
	}
	retime := extractUpdateIn(t, reg, "retime the alarm to 8")
	if retime.NewTime != "08:00" || retime.NewLabel != nil {
		t.Fatalf("retime request = %+v", retime)
	}
}
