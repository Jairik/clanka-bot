package calendar

import (
	"strings"
	"testing"
	"time"
)

// TestParserAllDayEvents verifies all-day events parse with AllDay: true and start/end boundaries.
func TestParserAllDayEvents(t *testing.T) {
	ics := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//EN
BEGIN:VEVENT
UID:allday-1@example.com
DTSTAMP:20261116T120000Z
DTSTART;VALUE=DATE:20261116
DTEND;VALUE=DATE:20261117
SUMMARY:Company Holiday
END:VEVENT
END:VCALENDAR`

	loc := time.UTC
	windowStart := time.Date(2026, 11, 16, 0, 0, 0, 0, loc)
	windowEnd := windowStart.AddDate(0, 0, 7)

	events, err := parseICS(strings.NewReader(ics), loc, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("parseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if !ev.AllDay {
		t.Errorf("AllDay = false, want true")
	}
	if ev.Summary != "Company Holiday" {
		t.Errorf("Summary = %q, want %q", ev.Summary, "Company Holiday")
	}
	wantStart := time.Date(2026, 11, 16, 0, 0, 0, 0, loc)
	wantEnd := time.Date(2026, 11, 17, 0, 0, 0, 0, loc)
	if !ev.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", ev.Start, wantStart)
	}
	if !ev.End.Equal(wantEnd) {
		t.Errorf("End = %v, want %v", ev.End, wantEnd)
	}
}

// TestParserTZID verifies TZID is honoured via embedded tzdata.
func TestParserTZID(t *testing.T) {
	ics := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//EN
BEGIN:VEVENT
UID:tzid-1@example.com
DTSTAMP:20261116T120000Z
DTSTART;TZID=America/New_York:20261116T093000
DTEND;TZID=America/New_York:20261116T103000
SUMMARY:Team Standup
END:VEVENT
END:VCALENDAR`

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	windowStart := time.Date(2026, 11, 16, 0, 0, 0, 0, loc)
	windowEnd := windowStart.AddDate(0, 0, 7)

	events, err := parseICS(strings.NewReader(ics), loc, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("parseICS: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Summary != "Team Standup" {
		t.Errorf("Summary = %q, want %q", ev.Summary, "Team Standup")
	}
	wantStart := time.Date(2026, 11, 16, 9, 30, 0, 0, loc)
	if !ev.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", ev.Start, wantStart)
	}
	if ev.Start.In(loc).Hour() != 9 || ev.Start.In(loc).Minute() != 30 {
		t.Errorf("Start in NY = %02d:%02d, want 09:30", ev.Start.In(loc).Hour(), ev.Start.In(loc).Minute())
	}
}

// TestParserWeeklyRRULEWithEXDATE verifies weekly recurring event expands and omits EXDATE.
func TestParserWeeklyRRULEWithEXDATE(t *testing.T) {
	// 4 Mondays: Nov 2, Nov 9, Nov 16, Nov 23. Exclude Nov 16.
	ics := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//EN
BEGIN:VEVENT
UID:rrule-exdate-1@example.com
DTSTAMP:20261102T120000Z
DTSTART:20261102T140000Z
DTEND:20261102T150000Z
RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4
EXDATE:20261116T140000Z
SUMMARY:Weekly Planning
END:VEVENT
END:VCALENDAR`

	loc := time.UTC
	windowStart := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	windowEnd := time.Date(2026, 11, 30, 0, 0, 0, 0, loc)

	events, err := parseICS(strings.NewReader(ics), loc, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("parseICS: %v", err)
	}
	// Expected: Nov 2, Nov 9, Nov 23 (3 occurrences, Nov 16 excluded).
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	dates := []int{2, 9, 23}
	for i, ev := range events {
		if ev.Start.Day() != dates[i] {
			t.Errorf("event[%d] day = %d, want %d", i, ev.Start.Day(), dates[i])
		}
	}
}

// TestParserEventsSpanningMidnight verifies an event starting on day N and ending on day N+1 overlaps both days.
func TestParserEventsSpanningMidnight(t *testing.T) {
	ics := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//EN
BEGIN:VEVENT
UID:midnight-1@example.com
DTSTAMP:20261116T120000Z
DTSTART:20261116T230000Z
DTEND:20261117T020000Z
SUMMARY:Late Night Hackathon
END:VEVENT
END:VCALENDAR`

	loc := time.UTC
	// Day 1: Nov 16
	day1Start := time.Date(2026, 11, 16, 0, 0, 0, 0, loc)
	day1End := day1Start.AddDate(0, 0, 1)

	events1, err := parseICS(strings.NewReader(ics), loc, day1Start, day1End)
	if err != nil {
		t.Fatalf("parseICS day 1: %v", err)
	}
	if len(events1) != 1 {
		t.Fatalf("day 1 got %d events, want 1", len(events1))
	}
	if events1[0].Summary != "Late Night Hackathon" {
		t.Errorf("day 1 summary = %q, want 'Late Night Hackathon'", events1[0].Summary)
	}

	// Day 2: Nov 17
	day2Start := time.Date(2026, 11, 17, 0, 0, 0, 0, loc)
	day2End := day2Start.AddDate(0, 0, 1)

	events2, err := parseICS(strings.NewReader(ics), loc, day2Start, day2End)
	if err != nil {
		t.Fatalf("parseICS day 2: %v", err)
	}
	if len(events2) != 1 {
		t.Fatalf("day 2 got %d events, want 1", len(events2))
	}
	if events2[0].Summary != "Late Night Hackathon" {
		t.Errorf("day 2 summary = %q, want 'Late Night Hackathon'", events2[0].Summary)
	}
}

// TestParserDSTTransitionDay verifies recurrence preserves local clock time across a DST transition.
func TestParserDSTTransitionDay(t *testing.T) {
	// America/New_York transitions from EDT to EST on Sunday, Nov 1, 2026 at 02:00.
	// Daily standup at 09:30 should stay 09:30 local time both before and after the transition.
	ics := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp.//EN
BEGIN:VEVENT
UID:dst-1@example.com
DTSTAMP:20261031T120000Z
DTSTART;TZID=America/New_York:20261031T093000
DTEND;TZID=America/New_York:20261031T100000
RRULE:FREQ=DAILY;COUNT=3
SUMMARY:Morning Sync
END:VEVENT
END:VCALENDAR`

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	windowStart := time.Date(2026, 10, 31, 0, 0, 0, 0, loc)
	windowEnd := time.Date(2026, 11, 3, 0, 0, 0, 0, loc)

	events, err := parseICS(strings.NewReader(ics), loc, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("parseICS: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}

	for i, ev := range events {
		inNY := ev.Start.In(loc)
		if inNY.Hour() != 9 || inNY.Minute() != 30 {
			t.Errorf("occurrence %d start time in NY = %02d:%02d, want 09:30", i, inNY.Hour(), inNY.Minute())
		}
	}
	// Day 0: Oct 31 is EDT (-04:00)
	// Day 1: Nov 1 is EST (-05:00)
	// Day 2: Nov 2 is EST (-05:00)
	if _, offset := events[0].Start.In(loc).Zone(); offset != -4*3600 {
		t.Errorf("Oct 31 offset = %d, want %d", offset, -4*3600)
	}
	if _, offset := events[1].Start.In(loc).Zone(); offset != -5*3600 {
		t.Errorf("Nov 1 offset = %d, want %d", offset, -5*3600)
	}
}
