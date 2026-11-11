package clock_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func TestTimeNowReturnsInjectedLocalTime(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 15, 4, 5, 0, time.UTC)
	tl := clock.New(func() time.Time { return fixed })
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result, err := reg.Execute(context.Background(), clock.Name, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := result.(clock.Out)
	if !ok {
		t.Fatalf("result is %T, want clock.Out", result)
	}
	if !out.Time.Equal(fixed) {
		t.Errorf("Time = %s, want %s", out.Time, fixed)
	}
}

func TestTimeNowSpeaksTwelveHourTime(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	tl := clock.New(func() time.Time { return fixed })
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := reg.Execute(context.Background(), clock.Name, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, ok := reg.Speak(clock.Name, result)
	if !ok {
		t.Fatal("Speak returned false")
	}
	if want := "It's 3:04 PM."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
}

func TestTimeNowIsFastPathEligible(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, clock.New(nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info, ok := reg.Info(clock.Name)
	if !ok {
		t.Fatal("clock tool not registered")
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
	if info.LocalOnly {
		t.Error("LocalOnly = true, want false")
	}
	if !info.HasSpeak {
		t.Error("HasSpeak = false, want true")
	}
	if len(info.Examples) == 0 {
		t.Error("Examples is empty")
	}
}
