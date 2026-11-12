package clock_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// fixedZoneReader supplies one effective timezone without a database.
type fixedZoneReader struct{ value string }

func (r fixedZoneReader) String(string) string { return r.value }

func zoneService(t *testing.T, name string) *tz.Service {
	t.Helper()
	service := tz.New("", nil)
	service.Bind(fixedZoneReader{value: name})
	return service
}

// TestCountdownUsesCalendarDaysAcrossDST is the DST regression: the answer is
// 271 calendar days, not a duration divided by 24 hours (which loses a day's
// fraction at the spring-forward boundary).
func TestCountdownUsesCalendarDaysAcrossDST(t *testing.T) {
	// 2026-03-28 23:30 UTC is 00:30 on 29 March in Berlin, the spring-forward
	// day. Christmas is 271 calendar days later.
	at := time.Date(2026, 3, 28, 23, 30, 0, 0, time.UTC)
	registry := clockRegistry(t, func() time.Time { return at }, zoneService(t, "Europe/Berlin"))
	out, err := executeClock[clock.CountdownOut](t, registry, clock.CountdownName, clock.CountdownIn{Event: "Christmas"})
	if err != nil {
		t.Fatalf("date.countdown: %v", err)
	}
	if out.Days != 271 {
		t.Fatalf("days = %d, want 271 calendar days", out.Days)
	}
	if out.Date != "2026-12-25" || out.Event != "Christmas" {
		t.Fatalf("out = %+v, want Christmas 2026-12-25", out)
	}
	if text, _ := registry.Speak(clock.CountdownName, out); text != "There are 271 days until Christmas." {
		t.Fatalf("Speak = %q", text)
	}
}

// TestCountdownUsesTimezoneSetting proves the live timezone setting changes
// the local "today", and therefore the day count, at one instant.
func TestCountdownUsesTimezoneSetting(t *testing.T) {
	// 15:04 UTC is 11 November in UTC and 12 November in Auckland (NZDT).
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	utcRegistry := clockRegistry(t, func() time.Time { return at }, nil)
	utcOut, err := executeClock[clock.CountdownOut](t, utcRegistry, clock.CountdownName, clock.CountdownIn{Event: "Christmas"})
	if err != nil {
		t.Fatalf("UTC countdown: %v", err)
	}
	if utcOut.Days != 44 {
		t.Fatalf("UTC days = %d, want 44", utcOut.Days)
	}

	aucklandRegistry := clockRegistry(t, func() time.Time { return at }, zoneService(t, "Pacific/Auckland"))
	nzOut, err := executeClock[clock.CountdownOut](t, aucklandRegistry, clock.CountdownName, clock.CountdownIn{Event: "Christmas"})
	if err != nil {
		t.Fatalf("Auckland countdown: %v", err)
	}
	if nzOut.Days != 43 {
		t.Fatalf("Auckland days = %d, want 43", nzOut.Days)
	}
}

// TestCountdownTodayTomorrowAndRollover checks the spoken boundary cases and
// the next-year rollover.
func TestCountdownTodayTomorrowAndRollover(t *testing.T) {
	tests := []struct {
		now   time.Time
		event string
		days  int
		date  string
		speak string
	}{
		{time.Date(2026, 12, 25, 9, 0, 0, 0, time.UTC), "Christmas", 0, "2026-12-25", "Christmas is today."},
		{time.Date(2026, 12, 24, 9, 0, 0, 0, time.UTC), "Christmas", 1, "2026-12-25", "Christmas is tomorrow."},
		{time.Date(2026, 12, 26, 9, 0, 0, 0, time.UTC), "Christmas", 364, "2027-12-25", "There are 364 days until Christmas."},
		{time.Date(2026, 12, 30, 9, 0, 0, 0, time.UTC), "New Year", 2, "2027-01-01", "There are 2 days until New Year's Day."},
		{time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), "Halloween", 30, "2026-10-31", "There are 30 days until Halloween."},
		{time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC), "Valentine's Day", 13, "2026-02-14", "There are 13 days until Valentine's Day."},
	}
	for _, test := range tests {
		t.Run(test.event, func(t *testing.T) {
			registry := clockRegistry(t, func() time.Time { return test.now }, nil)
			out, err := executeClock[clock.CountdownOut](t, registry, clock.CountdownName, clock.CountdownIn{Event: test.event})
			if err != nil {
				t.Fatalf("countdown: %v", err)
			}
			if out.Days != test.days || out.Date != test.date {
				t.Fatalf("out = %+v, want %d days to %s", out, test.days, test.date)
			}
			if text, _ := registry.Speak(clock.CountdownName, out); text != test.speak {
				t.Fatalf("Speak = %q, want %q", text, test.speak)
			}
		})
	}
}

// TestCountdownUnknownEventIsHelpful rejects unsupported events without a
// guessed date.
func TestCountdownUnknownEventIsHelpful(t *testing.T) {
	registry := clockRegistry(t, nil, nil)
	_, err := executeClock[clock.CountdownOut](t, registry, clock.CountdownName, clock.CountdownIn{Event: "my birthday"})
	if err == nil {
		t.Fatal("unknown event returned no error")
	}
	message := tool.FailureMessage(err)
	if !strings.Contains(message, "Christmas") || !strings.Contains(message, "Halloween") {
		t.Fatalf("error = %q, want the supported events", message)
	}
}

// TestExtractCountdown covers the accepted and rejected Fast path phrasings.
func TestExtractCountdown(t *testing.T) {
	registry := clockRegistry(t, nil, nil)
	for _, test := range []struct {
		utterance string
		event     string
	}{
		{"how many days until Christmas", "Christmas"},
		{"how many days till Christmas", "Christmas"},
		{"days until Christmas", "Christmas"},
		{"how many days until Halloween", "Halloween"},
		{"how many days until New Year", "New Year"},
		{"countdown to Christmas", "Christmas"},
		{"how many days before Valentine's Day", "Valentine's Day"},
		{"how many days until New Year's Day", "New Year's Day"},
		{"how many days are there until Christmas", "Christmas"},
	} {
		args, ok := registry.Extract(clock.CountdownName, test.utterance)
		if !ok {
			t.Fatalf("Extract(%q) failed", test.utterance)
		}
		var in clock.CountdownIn
		if err := json.Unmarshal(args, &in); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		if in.Event != test.event {
			t.Fatalf("Extract(%q) = %q, want %q", test.utterance, in.Event, test.event)
		}
	}
	for _, utterance := range []string{
		"what day is Christmas this year",
		"how many days until my birthday",
		"how many days until the weekend",
		"how long is a marathon",
		"how many days in a year",
	} {
		if _, ok := registry.Extract(clock.CountdownName, utterance); ok {
			t.Fatalf("Extract(%q) succeeded, want the Agent path", utterance)
		}
	}
}
