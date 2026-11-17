package timer_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// extractExtend extracts and executes timer.extend, returning the output and spoken reply.
func extractExtend(t *testing.T, reg *tool.Registry, utterance string) (timer.ExtendOut, string) {
	t.Helper()
	args, ok := reg.Extract(timer.ExtendName, utterance)
	if !ok {
		t.Fatalf("Extract(%q) failed", utterance)
	}
	result, err := reg.Execute(context.Background(), timer.ExtendName, args)
	if err != nil {
		t.Fatalf("Execute(%q): %v", utterance, err)
	}
	out, ok := result.(timer.ExtendOut)
	if !ok {
		t.Fatalf("Execute returned %T", result)
	}
	spoken, ok := reg.Speak(timer.ExtendName, result)
	if !ok {
		t.Fatal("Speak failed")
	}
	return out, spoken
}

// TestExtendByLabel verifies extending a specific timer by its label.
func TestExtendByLabel(t *testing.T) {
	reg, manager := labelRegistry(t)

	_, _ = extractSet(t, reg, "set a pasta timer for 10 minutes")
	_, _ = extractSet(t, reg, "set a tea timer for 5 minutes")

	extended, spoken := extractExtend(t, reg, "+5 minutes on the pasta timer")
	if extended.Label != "pasta" {
		t.Errorf("Label = %q, want pasta", extended.Label)
	}
	if extended.DurationSeconds != 600 {
		t.Errorf("DurationSeconds = %d, want original 600", extended.DurationSeconds)
	}
	if extended.RemainingSeconds != 900 {
		t.Errorf("RemainingSeconds = %d, want 900", extended.RemainingSeconds)
	}
	if want := fixedClock().Add(15 * time.Minute); !extended.FiresAt.Equal(want) {
		t.Errorf("FiresAt = %v, want %v", extended.FiresAt, want)
	}
	if want := "Pasta timer now has 15 minutes left."; spoken != want {
		t.Errorf("spoken = %q, want %q", spoken, want)
	}

	tea, ok := manager.Remaining("tea")
	if !ok {
		t.Fatal("tea timer missing")
	}
	if want := fixedClock().Add(5 * time.Minute); !tea.FiresAt.Equal(want) {
		t.Errorf("tea FiresAt = %v, want %v", tea.FiresAt, want)
	}
}

// TestExtendSoonestTimer verifies extending the soonest timer when no label is provided.
func TestExtendSoonestTimer(t *testing.T) {
	reg, manager := labelRegistry(t)

	_, _ = extractSet(t, reg, "set a tea timer for 5 minutes")
	_, _ = extractSet(t, reg, "set a pasta timer for 10 minutes")

	extended, spoken := extractExtend(t, reg, "add five minutes to the timer")
	if extended.Label != "tea" {
		t.Errorf("extended soonest Label = %q, want tea", extended.Label)
	}
	if extended.DurationSeconds != 300 {
		t.Errorf("DurationSeconds = %d, want original 300", extended.DurationSeconds)
	}
	if extended.RemainingSeconds != 600 {
		t.Errorf("RemainingSeconds = %d, want 600", extended.RemainingSeconds)
	}
	if want := "Tea timer now has 10 minutes left."; spoken != want {
		t.Errorf("spoken = %q, want %q", spoken, want)
	}

	pasta, ok := manager.Remaining("pasta")
	if !ok {
		t.Fatal("pasta timer missing")
	}
	if want := fixedClock().Add(10 * time.Minute); !pasta.FiresAt.Equal(want) {
		t.Errorf("pasta FiresAt = %v, want %v", pasta.FiresAt, want)
	}
}

// TestExtendUnlabelledTimer verifies speak output when an unlabelled timer is extended.
func TestExtendUnlabelledTimer(t *testing.T) {
	reg, _ := labelRegistry(t)

	_, _ = extractSet(t, reg, "set a timer for 10 minutes")
	extended, spoken := extractExtend(t, reg, "add five minutes to the timer")
	if extended.Label != "" {
		t.Errorf("Label = %q, want empty", extended.Label)
	}
	if extended.RemainingSeconds != 900 {
		t.Errorf("RemainingSeconds = %d, want 900", extended.RemainingSeconds)
	}
	if want := "Timer now has 15 minutes left."; spoken != want {
		t.Errorf("spoken = %q, want %q", spoken, want)
	}
}

// TestExtendSurvivesRestart asserts that updating fires_at persists in SQLite and
// is accurately recovered across process restarts without modifying duration_ms.
func TestExtendSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "timer.db")
	st := openTimerStore(t, dbPath)
	defer st.Close()

	clockNow := fixedClock()
	nowFn := func() time.Time { return clockNow }

	m1 := storedManager(t, st, nowFn, nil, nil)
	started, err := m1.Start("pasta", 5*time.Minute)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	extended, err := m1.Extend(started.ID, "", 5*time.Minute)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	wantFiresAt := clockNow.Add(10 * time.Minute)
	if !extended.FiresAt.Equal(wantFiresAt) {
		t.Fatalf("FiresAt = %v, want %v", extended.FiresAt, wantFiresAt)
	}
	m1.Close()

	// Simulate process restart with a new Manager reading from the same SQLite store.
	m2 := storedManager(t, st, nowFn, nil, nil)
	list := m2.List()
	if len(list) != 1 {
		t.Fatalf("recovered %d timers, want 1", len(list))
	}
	recovered := list[0]
	if recovered.ID != started.ID {
		t.Errorf("recovered ID = %s, want %s", recovered.ID, started.ID)
	}
	if recovered.Label != "pasta" {
		t.Errorf("recovered Label = %s, want pasta", recovered.Label)
	}
	if recovered.DurationSeconds != 300 {
		t.Errorf("recovered DurationSeconds = %d, want 300", recovered.DurationSeconds)
	}
	if !recovered.FiresAt.Equal(wantFiresAt) {
		t.Errorf("recovered FiresAt = %v, want %v", recovered.FiresAt, wantFiresAt)
	}
	if recovered.RemainingSeconds != 600 {
		t.Errorf("recovered RemainingSeconds = %d, want 600", recovered.RemainingSeconds)
	}
}

// TestExtendNoDoubleFire verifies that rescheduling a timer cancels the earlier callback
// so that it never fires at the old time or fires twice.
func TestExtendNoDoubleFire(t *testing.T) {
	var fireCount int64
	say := func(ctx context.Context, text string) error {
		atomic.AddInt64(&fireCount, 1)
		return nil
	}

	m := timer.NewManager(nil, say, discardLogger())
	defer m.Close()

	started, err := m.Start("egg", 30*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	// Extend the timer well past the original 30ms mark.
	if _, err := m.Extend(started.ID, "", 100*time.Millisecond); err != nil {
		t.Fatalf("Extend: %v", err)
	}

	// Wait past the original 30ms fire time.
	time.Sleep(40 * time.Millisecond)
	if count := atomic.LoadInt64(&fireCount); count != 0 {
		t.Fatalf("fireCount = %d at 50ms, want 0 (premature fire at old time)", count)
	}

	// Wait for the rescheduled timer to fire.
	waitUntil(t, "timer to fire at rescheduled time", func() bool {
		return atomic.LoadInt64(&fireCount) > 0
	})

	time.Sleep(30 * time.Millisecond)
	if count := atomic.LoadInt64(&fireCount); count != 1 {
		t.Fatalf("fireCount = %d, want exactly 1 (no double fire)", count)
	}
}

// TestExtendRingingTimerRefused verifies that attempting to extend an already-ringing
// timer is refused with a spoken tool error.
func TestExtendRingingTimerRefused(t *testing.T) {
	alert := newBlockingAlert()
	m := timer.NewManager(nil, alert.SayText, discardLogger())
	defer m.Close()

	started, err := m.Start("pasta", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForAlert(t, alert)

	// Attempt extend by label.
	_, err = m.Extend("", "pasta", 5*time.Minute)
	if err == nil {
		t.Fatal("Extend on ringing timer succeeded, want error")
	}
	toolErr, ok := err.(*tool.ToolError)
	if !ok {
		t.Fatalf("err type = %T, want *tool.ToolError", err)
	}
	if toolErr.Say == "" {
		t.Error("ToolError.Say is empty, want spoken refusal")
	}
	if want := "The pasta timer is already ringing."; toolErr.Say != want {
		t.Errorf("ToolError.Say = %q, want %q", toolErr.Say, want)
	}

	// Attempt extend by ID.
	_, err = m.Extend(started.ID, "", 5*time.Minute)
	if err == nil {
		t.Fatal("Extend by ID on ringing timer succeeded, want error")
	}
	toolErr, ok = err.(*tool.ToolError)
	if !ok {
		t.Fatalf("err type = %T, want *tool.ToolError", err)
	}
	if toolErr.Say == "" {
		t.Error("ToolError.Say by ID is empty, want spoken refusal")
	}

	m.Cancel("")
}

// TestExtendExtractCases tests Fast-path Extract against the natural phrases.
func TestExtendExtractCases(t *testing.T) {
	reg, _ := labelRegistry(t)

	tests := []struct {
		utterance string
		wantSecs  int
		wantLabel string
	}{
		{"+5 minutes on the pasta timer", 300, "pasta"},
		{"add five minutes to the timer", 300, ""},
		{"give the egg timer ten more minutes", 600, "egg"},
		{"give the timer five more minutes", 300, ""},
		{"add ten minutes to the pasta timer", 600, "pasta"},
		{"extend the timer by ten minutes", 600, ""},
		{"put five more minutes on the timer", 300, ""},
	}

	for _, tc := range tests {
		args, ok := reg.Extract(timer.ExtendName, tc.utterance)
		if !ok {
			t.Fatalf("Extract(%q) returned false", tc.utterance)
		}
		var in timer.ExtendIn
		if err := json.Unmarshal(args, &in); err != nil {
			t.Fatalf("Unmarshal(%q): %v", tc.utterance, err)
		}
		if in.ExtendSeconds != tc.wantSecs {
			t.Errorf("Extract(%q).ExtendSeconds = %d, want %d", tc.utterance, in.ExtendSeconds, tc.wantSecs)
		}
		if in.Label != tc.wantLabel {
			t.Errorf("Extract(%q).Label = %q, want %q", tc.utterance, in.Label, tc.wantLabel)
		}
	}
}
