package parse_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

func TestDuration(t *testing.T) {
	tests := []struct {
		text string
		def  time.Duration
		want time.Duration
		ok   bool
	}{
		{"five minutes", time.Minute, 5 * time.Minute, true},
		{"1 h 30", time.Minute, 90 * time.Minute, true},
		{"an hour and a half", time.Minute, 90 * time.Minute, true},
		{"one and a half hours", time.Minute, 90 * time.Minute, true},
		{"half an hour", time.Minute, 30 * time.Minute, true},
		{"a minute and a half", time.Minute, 90 * time.Second, true},
		{"set a timer for five minutes", time.Minute, 5 * time.Minute, true},
		{"90 seconds", time.Minute, 90 * time.Second, true},
		{"2 hours 15 minutes", time.Minute, 2*time.Hour + 15*time.Minute, true},
		{"1h30m", time.Minute, 90 * time.Minute, true},
		{"1:30", time.Minute, 90 * time.Minute, true},
		{"set a timer for 5", time.Minute, 5 * time.Minute, true},
		{"five", time.Minute, 5 * time.Minute, true},
		{"no duration here", time.Minute, 0, false},
		{"cancel the timer", time.Minute, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got, ok := parse.Duration(tt.text, tt.def)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("Duration(%q) = %s, %v; want %s, %v", tt.text, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNumber(t *testing.T) {
	tests := []struct {
		text string
		want int
		ok   bool
	}{
		{"turn the volume to 40", 40, true},
		{"forty percent", 40, true},
		{"set the volume to twenty five percent", 25, true},
		{"one hundred", 100, true},
		{"louder", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got, ok := parse.Number(tt.text)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("Number(%q) = %d, %v; want %d, %v", tt.text, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5 minutes"},
		{90 * time.Minute, "1 hour 30 minutes"},
		{90 * time.Second, "1 minute 30 seconds"},
		{45 * time.Second, "45 seconds"},
		{time.Minute, "1 minute"},
		{0, "0 seconds"},
	}
	for _, tt := range tests {
		if got := parse.HumanDuration(tt.d); got != tt.want {
			t.Errorf("HumanDuration(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
