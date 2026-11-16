package parse_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

// TestVolumeChangeReadsAbsoluteRelativeAndStep covers the shared speaker and
// music volume grammar: a number after "to"/"at" is absolute, a number after a
// bare direction is relative, and a bare direction is one step.
func TestVolumeChangeReadsAbsoluteRelativeAndStep(t *testing.T) {
	step := 10
	cases := []struct {
		text   string
		percent *int
		delta  int
		ok     bool
	}{
		// Absolute levels (with or without a direction word before "to").
		{"turn the volume down to 20", pi(20), 0, true},
		{"turn it up to 80", pi(80), 0, true},
		{"volume up to 70", pi(70), 0, true},
		{"set the volume to 5 percent", pi(5), 0, true},
		{"volume 100", pi(100), 0, true},
		{"turn the volume to 40", pi(40), 0, true},
		{"the music volume at 60", pi(60), 0, true},
		// Spoken word numbers ("zero", "twenty") parse the same as digits, and a
		// "to"/"at" word target stays absolute even when a direction word is
		// present.
		{"turn the volume to zero", pi(0), 0, true},
		{"set the volume to twenty", pi(20), 0, true},
		{"turn the volume down to twenty", pi(20), 0, true},
		// Relative deltas after a direction word without "to"/"at".
		{"turn the volume down by 30 percent", nil, -30, true},
		{"turn it up by 15", nil, 15, true},
		{"10 percent louder", nil, 10, true},
		{"make it quieter by 5", nil, -5, true},
		// Bare directions are one step.
		{"louder", nil, 10, true},
		{"quieter", nil, -10, true},
		{"raise the music volume", nil, 10, true},
		{"lower the music volume", nil, -10, true},
		{"turn the volume up", nil, 10, true},
		{"turn the volume down", nil, -10, true},
	}
	for _, tc := range cases {
		percent, delta, ok := parse.VolumeChange(tc.text, step)
		if ok != tc.ok {
			t.Errorf("VolumeChange(%q): ok = %v, want %v", tc.text, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if tc.percent == nil {
			if percent != nil {
				t.Errorf("VolumeChange(%q): percent = %v, want nil", tc.text, *percent)
			}
			if delta != tc.delta {
				t.Errorf("VolumeChange(%q): delta = %d, want %d", tc.text, delta, tc.delta)
			}
		} else {
			if percent == nil || *percent != *tc.percent {
				got := -1
				if percent != nil {
					got = *percent
				}
				t.Errorf("VolumeChange(%q): percent = %d, want %d", tc.text, got, *tc.percent)
			}
			if delta != 0 {
				t.Errorf("VolumeChange(%q): delta = %d, want 0", tc.text, delta)
			}
		}
	}
}

// TestVolumeChangeRefusesOutOfRangeAndNonVolume keeps an out-of-range number
// and a phrase with no number or direction out of the volume path so they fall
// through to the Agent. A zero relative ("down by 0") is also refused, because
// a step of zero is not a meaningful volume change.
func TestVolumeChangeRefusesOutOfRangeAndNonVolume(t *testing.T) {
	for _, text := range []string{
		"",
		"turn it down by 0",
		"set the volume to 101",
		"what time is it",
	} {
		if _, _, ok := parse.VolumeChange(text, 10); ok {
			t.Errorf("VolumeChange(%q) = ok, want false", text)
		}
	}
}

func pi(n int) *int { return &n }
