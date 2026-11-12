package parse_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

// TestClockReadsSpokenClockTimes covers the forms an Alarm request uses,
// including 12-hour suffixes, noon/midnight and a bare hour.
func TestClockReadsSpokenClockTimes(t *testing.T) {
	cases := []struct {
		text   string
		hour   int
		minute int
	}{
		{"wake me at 7", 7, 0},
		{"set an alarm for 6:30 on weekdays", 6, 30},
		{"set an alarm for 6:30 am", 6, 30},
		{"wake me at 7 am", 7, 0},
		{"wake me at 7am", 7, 0},
		{"set an alarm for 6:30pm", 18, 30},
		{"set an alarm for 6:30 pm", 18, 30},
		{"set an alarm for 12 am", 0, 0},
		{"set an alarm for 12 pm", 12, 0},
		{"set an alarm for 12:15 pm", 12, 15},
		{"set an alarm for 17:45", 17, 45},
		{"wake me at noon", 12, 0},
		{"wake me at midnight", 0, 0},
		{"cancel my 7 am alarm", 7, 0},
		{"alarm at 7 o'clock", 7, 0},
		{"set an alarm for 7 a.m.", 7, 0},
	}
	for _, tc := range cases {
		hour, minute, ok := parse.Clock(tc.text)
		if !ok || hour != tc.hour || minute != tc.minute {
			t.Errorf("Clock(%q) = %d:%02d, %v; want %d:%02d", tc.text, hour, minute, ok, tc.hour, tc.minute)
		}
	}
}

// TestClockRejectsDurationsAndInvalidTimes keeps a length or a bad hour out of
// the Alarm path: "10 minutes" is not ten o'clock.
func TestClockRejectsDurationsAndInvalidTimes(t *testing.T) {
	for _, text := range []string{
		"",
		"set a timer for 10 minutes",
		"set an alarm for 2 hours",
		"wake me at 25",
		"wake me at 13 pm",
		"wake me at 6:75",
		"wake me at seven",
	} {
		if hour, minute, ok := parse.Clock(text); ok {
			t.Errorf("Clock(%q) = %d:%02d, true; want no clock time", text, hour, minute)
		}
	}
}
