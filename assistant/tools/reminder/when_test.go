package reminder

import (
	"fmt"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

// TestParseSetInputReadsNaturalDurationPhrases covers the duration form: the
// time span is understood and only the thing to be reminded about remains.
func TestParseSetInputReadsNaturalDurationPhrases(t *testing.T) {
	cases := []struct {
		text     string
		message  string
		duration time.Duration
	}{
		{"remind me in 20 minutes to check the oven", "check the oven", 20 * time.Minute},
		{"remind me in ten minutes to check the oven", "check the oven", 10 * time.Minute},
		{"set a reminder for 30 minutes to stretch", "stretch", 30 * time.Minute},
		{"remind me to take the bins out in five minutes", "take the bins out", 5 * time.Minute},
		{"remind me in an hour and a half to switch the laundry", "switch the laundry", 90 * time.Minute},
		{"can you remind me in 90 seconds to check the pasta", "check the pasta", 90 * time.Second},
		{"remind me in 1 hour 30 minutes to call the plumber", "call the plumber", 90 * time.Minute},
		{"please remind me in 2 hours to water the plants", "water the plants", 2 * time.Hour},
		{"remind me to check the oven in 20 minutes.", "check the oven", 20 * time.Minute},
		{"remind me in 20 minutes to meet at 5", "meet at 5", 20 * time.Minute},
	}
	for _, tc := range cases {
		in, ok := parseSetInput(tc.text)
		if !ok {
			t.Errorf("parseSetInput(%q) = false, want a duration reminder", tc.text)
			continue
		}
		if in.DurationSeconds != int(tc.duration/time.Second) {
			t.Errorf("%q duration = %ds, want %ds", tc.text, in.DurationSeconds, int(tc.duration/time.Second))
		}
		if in.At != "" {
			t.Errorf("%q produced clock time %q, want a duration", tc.text, in.At)
		}
		if in.Message != tc.message {
			t.Errorf("%q message = %q, want %q", tc.text, in.Message, tc.message)
		}
	}
}

// TestParseSetInputReadsNaturalClockPhrases covers the clock form, including
// the 12-hour phrasing a user actually speaks.
func TestParseSetInputReadsNaturalClockPhrases(t *testing.T) {
	cases := []struct {
		text     string
		message  string
		at       string
		tomorrow bool
	}{
		{"remind me at 5 pm to call mum", "call mum", "17:00", false},
		{"remind me at 5pm to call mum", "call mum", "17:00", false},
		{"remind me at 17:30 to leave for work", "leave for work", "17:30", false},
		{"remind me at 7 am to take my pills", "take my pills", "07:00", false},
		{"set a reminder for 6:30 to feed the cat", "feed the cat", "06:30", false},
		{"remind me tomorrow at 9 am to call the dentist", "call the dentist", "09:00", true},
		{"remind me at noon to eat lunch", "eat lunch", "12:00", false},
		{"remind me at midnight to put the bins out", "put the bins out", "00:00", false},
		{"remind me at 12 am to check the alarm", "check the alarm", "00:00", false},
		{"remind me at 12 pm to take a break", "take a break", "12:00", false},
		{"remind me at 5 to call mum", "call mum", "05:00", false},
		{"remind me 5 pm call mum", "call mum", "17:00", false},
		{"remind me to call mum at 5 pm", "call mum", "17:00", false},
		{"remind me at 5 pm to call in 20 minutes", "call in 20 minutes", "17:00", false},
	}
	for _, tc := range cases {
		in, ok := parseSetInput(tc.text)
		if !ok {
			t.Errorf("parseSetInput(%q) = false, want a clock reminder", tc.text)
			continue
		}
		if in.At != tc.at {
			t.Errorf("%q at = %q, want %q", tc.text, in.At, tc.at)
		}
		if in.Tomorrow != tc.tomorrow {
			t.Errorf("%q tomorrow = %v, want %v", tc.text, in.Tomorrow, tc.tomorrow)
		}
		if in.DurationSeconds != 0 {
			t.Errorf("%q produced duration %ds, want a clock time", tc.text, in.DurationSeconds)
		}
		if in.Message != tc.message {
			t.Errorf("%q message = %q, want %q", tc.text, in.Message, tc.message)
		}
	}
}

// TestParseSetInputRejectsStatementsAndIncompleteRequests keeps the Fast path
// honest: a reminder without a time or a message is not a Tool call. A
// statement about a reminder on another device is not one either.
func TestParseSetInputRejectsStatementsAndIncompleteRequests(t *testing.T) {
	for _, text := range []string{
		"remind me to buy milk",
		"remind me at 5",
		"remind me in 20 minutes",
		"set a reminder",
		"set a reminder for 10 minutes",
		"reminder that the meeting is at noon",
		"reminder app settings",
		"I set a reminder on my phone",
		"what reminders does my phone have",
		"how do I set a reminder on my phone",
		"tell me a joke",
		"set a timer for 10 minutes",
	} {
		if in, ok := parseSetInput(text); ok {
			t.Errorf("parseSetInput(%q) = %+v, true; want rejected", text, in)
		}
	}
}

// TestParseSetInputTonorrowOnlyFuture covers the clock policy seam: a
// bare "tomorrow" is carried into the input and the manager resolves the day.
func TestParseSetInputTomorrowOnlyFuture(t *testing.T) {
	in, ok := parseSetInput("remind me tomorrow at 7 am to take the bins out")
	if !ok || !in.Tomorrow || in.At != "07:00" {
		t.Fatalf("parseSetInput = %+v, %v; want tomorrow at 07:00", in, ok)
	}
}

// TestCancellationQueryExtractsTheMessage covers the cancel forms the Tool
// Examples use, including a full message phrase and a single word.
func TestCancellationQueryExtractsTheMessage(t *testing.T) {
	cases := map[string]string{
		"cancel the reminder":                  "",
		"cancel my reminder":                   "",
		"cancel all reminders":                 "",
		"cancel the oven reminder":             "oven",
		"cancel the reminder to call mum":      "call mum",
		"delete my reminder about the dentist": "the dentist",
		"forget the reminder for the oven":     "the oven",
		"remove the reminder to water plants":  "water plants",
		"clear reminders":                      "",
		"cancel every reminder":                "",
		"delete each reminder":                 "",
	}
	for text, want := range cases {
		if got := cancellationQuery(text); got != want {
			t.Errorf("cancellationQuery(%q) = %q, want %q", text, got, want)
		}
	}
	if got := cancellationQuery("stop the timer"); got != "" {
		t.Errorf("cancellationQuery(stop the timer) = %q, want rejected", got)
	}
}

// TestClockTimePolicyIsStrictlyFuture pins the policy shared with Alarms: the
// named clock time today when it is still ahead, otherwise tomorrow.
func TestClockTimePolicyIsStrictlyFuture(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		// 16:04 Berlin, 17:00 today.
		{time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC), time.Date(2026, 11, 11, 17, 0, 0, 0, berlin)},
		// Exactly 17:00 is not future: the next occurrence is tomorrow.
		{time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC), time.Date(2026, 11, 12, 17, 0, 0, 0, berlin)},
		// 17:00:01 is past: tomorrow.
		{time.Date(2026, 11, 11, 16, 0, 1, 0, time.UTC), time.Date(2026, 11, 12, 17, 0, 0, 0, berlin)},
	}
	for _, tc := range cases {
		got := nextClockTime(tc.now, 17, 0, 0, berlin)
		if !got.Equal(tc.want) {
			t.Errorf("nextClockTime(%s) = %s, want %s", tc.now, got.In(berlin), tc.want.In(berlin))
		}
	}
	// An explicit tomorrow stays tomorrow even when the hour is still ahead.
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, berlin)
	got := nextClockTime(now, 17, 0, 1, berlin)
	if want := time.Date(2026, 11, 12, 17, 0, 0, 0, berlin); !got.Equal(want) {
		t.Fatalf("tomorrow at 17:00 = %s, want %s", got.In(berlin), want.In(berlin))
	}
}

// TestClockTimeDSTUsesTheAlarmResolver keeps Reminder and Alarm identical on
// the two ambiguous days: 02:30 on the spring-forward day becomes 03:30, and
// 01:30 on the fall-back day is the first occurrence.
func TestClockTimeDSTUsesTheAlarmResolver(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	gapNow := time.Date(2026, time.March, 8, 5, 0, 0, 0, time.UTC) // 00:00 EST
	gap := nextClockTime(gapNow, 2, 30, 0, loc)
	if want := time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC); !gap.Equal(want) {
		t.Fatalf("spring-forward 02:30 = %s, want %s", gap, want)
	}
	overlapNow := time.Date(2026, time.November, 1, 4, 0, 0, 0, time.UTC) // 00:00 EDT
	overlap := nextClockTime(overlapNow, 1, 30, 0, loc)
	if want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC); !overlap.Equal(want) {
		t.Fatalf("fall-back 01:30 = %s, want the first occurrence %s", overlap, want)
	}
}

// TestClockParserMatchesTheAlarmParser keeps the two spoken clock readers in
// sync: a Reminder must read every form an Alarm accepts the same way.
func TestClockParserMatchesTheAlarmParser(t *testing.T) {
	for _, form := range []string{
		"7", "6:30", "6:30 am", "7 am", "7am", "6:30pm", "6:30 pm",
		"12 am", "12 pm", "12:15 pm", "17:45", "noon", "midnight",
		"7 o'clock", "7 a.m.",
	} {
		hour, minute, ok := parse.Clock("remind me at " + form + " to check")
		if !ok {
			t.Errorf("parse.Clock(%q) rejected the form", form)
			continue
		}
		in, ok := parseSetInput("remind me at " + form + " to check")
		if !ok {
			t.Errorf("parseSetInput(%q) rejected the form", form)
			continue
		}
		want := fmt.Sprintf("%02d:%02d", hour, minute)
		if in.At != want {
			t.Errorf("%q reminder clock = %q, Alarm clock = %q", form, in.At, want)
		}
	}
}
