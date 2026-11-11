package tool_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestWireNameConvertsDotToDoubleUnderscore(t *testing.T) {
	got, err := tool.WireName("calendar.get_events")
	if err != nil {
		t.Fatalf("WireName returned error: %v", err)
	}
	if want := "calendar__get_events"; got != want {
		t.Fatalf("WireName = %q, want %q", got, want)
	}
}

func TestWireNameRejectsInvalidCanonicalNames(t *testing.T) {
	for _, name := range []string{
		"",
		"time",
		"time.now.extra",
		"Time.now",
		"time.Now",
		"time.",
		".now",
		"time.now!",
		"time..now",
		"time__now",
		"time.now__x",
		"time. now",
	} {
		if _, err := tool.WireName(name); err == nil {
			t.Errorf("WireName(%q) succeeded, want error", name)
		}
	}
}

func TestCanonicalNameConvertsDoubleUnderscoreToDot(t *testing.T) {
	got, err := tool.CanonicalName("calendar__get_events")
	if err != nil {
		t.Fatalf("CanonicalName returned error: %v", err)
	}
	if want := "calendar.get_events"; got != want {
		t.Fatalf("CanonicalName = %q, want %q", got, want)
	}
}

func TestCanonicalNameRejectsInvalidWireNames(t *testing.T) {
	for _, name := range []string{
		"",
		"time",
		"time.now",
		"time__now__x",
		"time____now",
		"__time__now",
		"time__",
		"Time__now",
		"time__Now",
		"time__",
	} {
		if _, err := tool.CanonicalName(name); err == nil {
			t.Errorf("CanonicalName(%q) succeeded, want error", name)
		}
	}
}
