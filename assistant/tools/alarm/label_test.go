package alarm

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestLabelForAlarm covers the natural label forms and the non-labels.
func TestLabelForAlarm(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"set a work alarm for 7:30", "work"},
		{"set an alarm called gym for 6:30", "gym"},
		{"cancel my work alarm", "work"},
		{"wake me at 7", ""},
		{"cancel my 7 am alarm", ""},
		{"cancel my wake up alarm", ""},
		{"delete my 8 o'clock alarm", ""},
		{"set an alarm for 7", ""},
	}
	for _, tc := range cases {
		if got := labelForAlarm(tc.text); got != tc.want {
			t.Errorf("labelForAlarm(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// TestCleanAlarmLabelKeepsRunesIntact asserts a multi-byte label whose 41st rune
// straddles byte 40 is capped on rune boundaries, never splitting a rune.
func TestCleanAlarmLabelKeepsRunesIntact(t *testing.T) {
	label := strings.Repeat("a", 39) + "日" + "b"
	got := cleanAlarmLabel(label)
	if !utf8.ValidString(got) {
		t.Fatalf("cleanAlarmLabel produced invalid UTF-8: %q", got)
	}
	if len([]rune(got)) != 40 {
		t.Errorf("cleanAlarmLabel kept %d runes, want 40", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "日") {
		t.Errorf("cleanAlarmLabel dropped the boundary rune: %q", got)
	}
}

// TestCleanAlarmLabelCapsAscii verifies a long ASCII label is still capped at 40.
func TestCleanAlarmLabelCapsAscii(t *testing.T) {
	got := cleanAlarmLabel(strings.Repeat("a", 45))
	if len([]rune(got)) != 40 {
		t.Errorf("cleanAlarmLabel kept %d runes, want 40", len([]rune(got)))
	}
	if got != strings.Repeat("a", 40) {
		t.Errorf("cleanAlarmLabel cap mismatch: %q", got)
	}
}
