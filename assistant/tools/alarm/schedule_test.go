package alarm

import (
	"strings"
	"testing"
	"time"
)

// newYork loads the DST test zone; the package embeds tzdata through internal/tz.
func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	return loc
}

// TestNextOnceTodayOrTomorrow pins the one-off rule: today when the clock time
// is still ahead, tomorrow when it has passed, and tomorrow when asked.
func TestNextOnceTodayOrTomorrow(t *testing.T) {
	loc := newYork(t)
	morning := time.Date(2026, 11, 12, 6, 0, 0, 0, loc)
	if got := nextOnce(morning, 7, 0, loc, false); !got.Equal(time.Date(2026, 11, 12, 7, 0, 0, 0, loc)) {
		t.Errorf("before the clock time = %s, want today 07:00", got)
	}
	afternoon := time.Date(2026, 11, 12, 8, 0, 0, 0, loc)
	if got := nextOnce(afternoon, 7, 0, loc, false); !got.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, loc)) {
		t.Errorf("after the clock time = %s, want tomorrow 07:00", got)
	}
	if got := nextOnce(morning, 7, 0, loc, true); !got.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, loc)) {
		t.Errorf("explicit tomorrow = %s, want tomorrow 07:00", got)
	}
}

// TestNextOccurrenceRules covers daily, weekdays, weekends and chosen days.
// 2026-11-13 is a Friday.
func TestNextOccurrenceRules(t *testing.T) {
	loc := newYork(t)
	fridayMorning := time.Date(2026, 11, 13, 8, 0, 0, 0, loc)
	cases := []struct {
		name string
		spec scheduleSpec
		want time.Time
	}{
		{"daily", scheduleSpec{schedule: Daily}, time.Date(2026, 11, 14, 7, 0, 0, 0, loc)},
		{"weekdays", scheduleSpec{schedule: Weekdays}, time.Date(2026, 11, 16, 7, 0, 0, 0, loc)},
		{"weekends", scheduleSpec{schedule: Weekends}, time.Date(2026, 11, 14, 7, 0, 0, 0, loc)},
		{
			"chosen monday and wednesday",
			scheduleSpec{schedule: Chosen, days: 1<<uint(time.Monday) | 1<<uint(time.Wednesday)},
			time.Date(2026, 11, 16, 7, 0, 0, 0, loc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextOccurrence(tc.spec, 7, 0, loc, fridayMorning); !got.Equal(tc.want) {
				t.Errorf("next = %s, want %s", got, tc.want)
			}
		})
	}
	// A clock time later today still wins over the next day.
	if got := nextOccurrence(scheduleSpec{schedule: Daily}, 9, 0, loc, fridayMorning); !got.Equal(time.Date(2026, 11, 13, 9, 0, 0, 0, loc)) {
		t.Errorf("later today = %s, want today 09:00", got)
	}
}

// TestResolveWallDSTGapAndOverlap documents the deterministic DST policy. A
// spring-forward wall time fires after the jump; a fall-back wall time fires at
// its first occurrence.
func TestResolveWallDSTGapAndOverlap(t *testing.T) {
	loc := newYork(t)
	// 2026-03-08 02:30 does not exist in New York; 02:00 EST jumps to 03:00
	// EDT. The post-transition reading is 03:30 EDT (07:30 UTC).
	gap := resolveWall(2026, time.March, 8, 2, 30, loc)
	if want := time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC); !gap.Equal(want) {
		t.Errorf("gap = %s (%s), want %s", gap, gap.In(loc), want)
	}
	if local := gap.In(loc); local.Hour() != 3 || local.Minute() != 30 {
		t.Errorf("gap local = %s, want 03:30", local)
	}

	// 2026-11-01 01:30 happens twice; the first (EDT, 05:30 UTC) wins.
	overlap := resolveWall(2026, time.November, 1, 1, 30, loc)
	if want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC); !overlap.Equal(want) {
		t.Errorf("overlap = %s (%s), want the first occurrence %s", overlap, overlap.In(loc), want)
	}

	// A normal wall time is exact.
	normal := resolveWall(2026, time.November, 12, 7, 15, loc)
	if want := time.Date(2026, time.November, 12, 7, 15, 0, 0, loc); !normal.Equal(want) {
		t.Errorf("normal = %s, want %s", normal, want)
	}
}

// TestParseScheduleText reads the repeat rule from real phrasings.
func TestParseScheduleText(t *testing.T) {
	cases := []struct {
		text string
		want Schedule
		days []string
	}{
		{"wake me at 7", Once, nil},
		{"set an alarm for 6:30 on weekdays", Weekdays, nil},
		{"wake me at 7 every day", Daily, nil},
		{"set an alarm for 8 on weekends", Weekends, nil},
		{"set an alarm for 6:45 on monday", Chosen, []string{"monday"}},
		{"set an alarm for 7 on tuesdays and thursdays", Chosen, []string{"tuesday", "thursday"}},
		{"set an alarm for 7 on mon, wed and fri", Chosen, []string{"monday", "wednesday", "friday"}},
	}
	for _, tc := range cases {
		got, days := parseScheduleText(tc.text)
		if got != tc.want || strings.Join(days, ",") != strings.Join(tc.days, ",") {
			t.Errorf("parseScheduleText(%q) = %s %v, want %s %v", tc.text, got, days, tc.want, tc.days)
		}
	}
}

// TestNormalizeScheduleRejectsBadCombinations keeps the Tool contract honest.
func TestNormalizeScheduleRejectsBadCombinations(t *testing.T) {
	if _, _, err := normalizeSchedule("daily", []string{"monday"}); err == nil {
		t.Error("daily plus weekdays was accepted")
	}
	if _, _, err := normalizeSchedule("chosen", nil); err == nil {
		t.Error("chosen without weekdays was accepted")
	}
	if _, _, err := normalizeSchedule("sometimes", nil); err == nil {
		t.Error("unknown schedule was accepted")
	}
	spec, days, err := normalizeSchedule("", []string{"monday", "wednesday"})
	if err != nil || spec.schedule != Chosen || strings.Join(days, ",") != "monday,wednesday" {
		t.Fatalf("weekdays without a schedule = %+v %v %v", spec, days, err)
	}
}
