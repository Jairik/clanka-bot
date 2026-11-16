package timer

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCleanLabelKeepsRunesIntact asserts a multi-byte label whose 41st rune
// straddles byte 40 is capped on rune boundaries, never splitting a rune.
func TestCleanLabelKeepsRunesIntact(t *testing.T) {
	// 39 ASCII runes + a 3-byte "日" at rune 40 + a trailing rune. By bytes,
	// "日" straddles byte 40, so a byte slice would produce invalid UTF-8.
	label := strings.Repeat("a", 39) + "日" + "b"
	got := cleanLabel(label)
	if !utf8.ValidString(got) {
		t.Fatalf("cleanLabel produced invalid UTF-8: %q", got)
	}
	if len([]rune(got)) != 40 {
		t.Errorf("cleanLabel kept %d runes, want 40", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "日") {
		t.Errorf("cleanLabel dropped the boundary rune: %q", got)
	}
}

// TestCleanLabelCapsAscii verifies a long ASCII label is still capped at 40.
func TestCleanLabelCapsAscii(t *testing.T) {
	got := cleanLabel(strings.Repeat("a", 45))
	if len([]rune(got)) != 40 {
		t.Errorf("cleanLabel kept %d runes, want 40", len([]rune(got)))
	}
	if got != strings.Repeat("a", 40) {
		t.Errorf("cleanLabel cap mismatch: %q", got)
	}
}
