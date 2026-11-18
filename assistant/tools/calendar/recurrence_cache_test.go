package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Multi-day occurrences remain visible through their last local calendar day.
func TestRecurringEventsOverlapTheWindowAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		when string
		hour int
	}{
		{"DTSTART;VALUE=DATE:20260307\nDTEND;VALUE=DATE:20260310", 0},
		{"DTSTART;TZID=America/New_York:20260307T100000\nDURATION:P3D", 10},
	} {
		feed := "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:conference\n" +
			event.when + "\nRRULE:FREQ=WEEKLY;COUNT=2\nSUMMARY:Conference\nEND:VEVENT\nEND:VCALENDAR"
		start := time.Date(2026, 3, 16, 0, 0, 0, 0, loc)
		end := time.Date(2026, 3, 17, event.hour, 0, 0, 0, loc)
		events, err := parseICS(strings.NewReader(feed), loc, start, start.AddDate(0, 0, 1))
		if err != nil || len(events) != 1 || !events[0].End.Equal(end) {
			t.Fatalf("recurring event for %s: %+v, %v; want end %v", event.when, events, err, end)
		}
		events, err = parseICS(strings.NewReader(feed), loc, end, end.AddDate(0, 0, 1))
		if err != nil || len(events) != 0 {
			t.Fatalf("events after end: %+v, %v", events, err)
		}
	}
}

// TestMovedOccurrenceReplacesOriginal checks a detached override replaces its original.
func TestMovedOccurrenceReplacesOriginal(t *testing.T) {
	feed := `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:meeting
DTSTART:20261117T090000Z
DTEND:20261117T100000Z
RRULE:FREQ=DAILY;COUNT=2
SUMMARY:Meeting
END:VEVENT
BEGIN:VEVENT
UID:meeting
RECURRENCE-ID:20261117T090000Z
DTSTART:20261117T110000Z
DTEND:20261117T120000Z
SUMMARY:Moved meeting
END:VEVENT
END:VCALENDAR`
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	events, err := parseICS(strings.NewReader(feed), time.UTC, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events: %+v; want only moved occurrence", len(events), events)
	}
}

// TestCancelledEventIsExcluded checks cancelled feed entries stay out of answers.
func TestCancelledEventIsExcluded(t *testing.T) {
	feed := `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:cancelled
DTSTART:20261117T090000Z
DTEND:20261117T100000Z
STATUS:CANCELLED
SUMMARY:Cancelled meeting
END:VEVENT
END:VCALENDAR`
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	events, err := parseICS(strings.NewReader(feed), time.UTC, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("got %+v; want no cancelled events", events)
	}
}

// TestCacheRejectsDifferentInsecureFeed checks a different feed cannot reuse old cached titles.
func TestCacheRejectsDifferentInsecureFeed(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	c := newClient(nil, func() time.Time { return now })
	c.cachedAt = now
	c.cachedEvents = []EventInstance{{Summary: "Old private feed", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}}
	events, err := c.Fetch(context.Background(), "http://invalid.example/new-feed", time.UTC, now, now.AddDate(0, 0, 1))
	if err == nil && len(events) > 0 {
		t.Fatalf("different insecure feed returned old private events: %+v", events)
	}
}

// Cancelled overrides suppress the master even without DTSTART or component ordering guarantees.
func TestCancelledRecurrenceWithoutStart(t *testing.T) {
	feed := `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:meeting
RECURRENCE-ID:20261117T090000Z
STATUS:CANCELLED
END:VEVENT
BEGIN:VEVENT
UID:meeting
DTSTART:20261117T090000Z
DTEND:20261117T100000Z
RRULE:FREQ=DAILY;COUNT=2
SUMMARY:Meeting
END:VEVENT
END:VCALENDAR`
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	events, err := parseICS(strings.NewReader(feed), time.UTC, start, start.AddDate(0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Start.Day() != 18 {
		t.Fatalf("events = %+v, want only November 18", events)
	}
}

// Live feed, timezone, and query-window changes must fetch a newly interpreted calendar.
func TestCacheTracksFeedZoneAndWindow(t *testing.T) {
	requests := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:one\nDTSTART;VALUE=DATE:20261117\nSUMMARY:%s\nEND:VEVENT\nEND:VCALENDAR\n", r.URL.Path)
	}))
	defer server.Close()
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	c := newClient(nil, func() time.Time { return now })
	end := now.AddDate(0, 0, 2)
	events, err := c.Fetch(t.Context(), server.URL+"/old", time.UTC, now, end)
	if err != nil || len(events) != 1 {
		t.Fatalf("initial: %+v, %v", events, err)
	}
	_, err = c.Fetch(t.Context(), server.URL+"/old", time.UTC, now, end)
	if err != nil || requests != 1 {
		t.Fatalf("cache hit: requests=%d err=%v", requests, err)
	}
	events, err = c.Fetch(t.Context(), server.URL+"/new", time.UTC, now, end)
	if err != nil || len(events) != 1 || events[0].Summary != "/new" || requests != 2 {
		t.Fatalf("feed change: %+v, %v requests=%d", events, err, requests)
	}
	zone := time.FixedZone("test-zone", -5*60*60)
	events, err = c.Fetch(t.Context(), server.URL+"/new", zone, now, end)
	if err != nil || len(events) != 1 || events[0].Start.Hour() != 0 || events[0].Start.Location() != zone || requests != 3 {
		t.Fatalf("zone change: %+v, %v requests=%d", events, err, requests)
	}
	_, err = c.Fetch(t.Context(), server.URL+"/new", zone, now, end.AddDate(0, 0, 1))
	if err != nil || requests != 4 {
		t.Fatalf("window change: err=%v requests=%d", err, requests)
	}
	fail = true
	now = now.Add(cacheTTL)
	events, err = c.Fetch(t.Context(), server.URL+"/new", zone, now.Add(-cacheTTL), end.AddDate(0, 0, 1))
	if err != nil || len(events) != 1 {
		t.Fatalf("same identity stale fallback: %+v, %v", events, err)
	}
	events, err = c.Fetch(t.Context(), server.URL+"/other", zone, now, end)
	if err == nil || len(events) != 0 {
		t.Fatalf("different feed stale fallback: %+v, %v", events, err)
	}
}
