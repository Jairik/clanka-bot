package timer_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// fixedClock is the injected clock every label and remaining test uses.
func fixedClock() time.Time {
	return time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
}

// labelRegistry wires the timer Tools over a manager with the injected clock.
func labelRegistry(t *testing.T) (*tool.Registry, *timer.Manager) {
	t.Helper()
	m := timer.NewManager(func() time.Time { return fixedClock() }, nil, nil)
	return timerRegistry(t, m), m
}

// timerRegistry registers the timer Tools on a fresh registry.
func timerRegistry(t *testing.T, m *timer.Manager) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := timer.Register(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// extractSet extracts and executes timer.set, returning the spoken reply.
func extractSet(t *testing.T, reg *tool.Registry, utterance string) (timer.SetOut, string) {
	t.Helper()
	args, ok := reg.Extract(timer.SetName, utterance)
	if !ok {
		t.Fatalf("Extract(%q) failed", utterance)
	}
	result, err := reg.Execute(context.Background(), timer.SetName, args)
	if err != nil {
		t.Fatalf("Execute(%q): %v", utterance, err)
	}
	out, ok := result.(timer.SetOut)
	if !ok {
		t.Fatalf("Execute returned %T", result)
	}
	spoken, ok := reg.Speak(timer.SetName, result)
	if !ok {
		t.Fatal("Speak failed")
	}
	return out, spoken
}

// TestSetLabelAndRemainingThroughParseAndRegistry is the third acceptance
// criterion at the real parse/registry seam: "set a pasta timer for 10 minutes"
// then "how long is left on the pasta timer".
func TestSetLabelAndRemainingThroughParseAndRegistry(t *testing.T) {
	reg, manager := labelRegistry(t)

	created, spoken := extractSet(t, reg, "set a pasta timer for 10 minutes")
	if created.Label != "pasta" || created.DurationSeconds != 600 {
		t.Fatalf("created = %+v, want a 10 minute pasta timer", created)
	}
	if want := "Pasta timer set for 10 minutes."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}

	args, ok := reg.Extract(timer.RemainingName, "how long is left on the pasta timer")
	if !ok {
		t.Fatal("remaining Extract failed")
	}
	var in timer.RemainingIn
	if err := json.Unmarshal(args, &in); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if in.Label != "pasta" {
		t.Fatalf("remaining label = %q, want pasta", in.Label)
	}
	result, err := reg.Execute(context.Background(), timer.RemainingName, args)
	if err != nil {
		t.Fatalf("remaining Execute: %v", err)
	}
	spoken, ok = reg.Speak(timer.RemainingName, result)
	if !ok {
		t.Fatal("remaining Speak failed")
	}
	if want := "The pasta timer has 10 minutes left."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
	if got := len(manager.List()); got != 1 {
		t.Fatalf("timers = %d, want 1", got)
	}
	manager.Cancel("")
}

// TestRemainingWithoutLabelUsesTheSoonestTimer covers the unlabelled lookup.
func TestRemainingWithoutLabelUsesTheSoonestTimer(t *testing.T) {
	reg, manager := labelRegistry(t)
	manager.Start("pasta", 10*time.Minute)
	manager.Start("", 5*time.Minute)

	args, ok := reg.Extract(timer.RemainingName, "how much time is left on the timer")
	if !ok {
		t.Fatal("Extract failed")
	}
	result, err := reg.Execute(context.Background(), timer.RemainingName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, _ := reg.Speak(timer.RemainingName, result)
	if want := "Your timer has 5 minutes left."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
	manager.Cancel("")
}

// TestRemainingUnknownLabelFailsClosed keeps a lookup from silently answering
// about the wrong timer.
func TestRemainingUnknownLabelFailsClosed(t *testing.T) {
	reg, manager := labelRegistry(t)
	manager.Start("pasta", 10*time.Minute)

	args, ok := reg.Extract(timer.RemainingName, "how long is left on the cake timer")
	if !ok {
		t.Fatal("Extract failed")
	}
	_, err := reg.Execute(context.Background(), timer.RemainingName, args)
	if err == nil {
		t.Fatal("Execute succeeded for an unknown label")
	}
	if got, want := tool.FailureMessage(err), "There is no cake timer."; got != want {
		t.Fatalf("FailureMessage = %q, want %q", got, want)
	}
	manager.Cancel("")
}

// TestCancelByLabelThroughParse removes only the named timer.
func TestCancelByLabelThroughParse(t *testing.T) {
	reg, manager := labelRegistry(t)
	manager.Start("pasta", 10*time.Minute)
	manager.Start("tea", 5*time.Minute)

	args, ok := reg.Extract(timer.CancelName, "cancel the pasta timer")
	if !ok {
		t.Fatal("Extract failed")
	}
	result, err := reg.Execute(context.Background(), timer.CancelName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := result.(timer.CancelOut)
	if !ok || out.Cancelled != 1 {
		t.Fatalf("result = %#v, want one cancelled", result)
	}
	list := manager.List()
	if len(list) != 1 || list[0].Label != "tea" {
		t.Fatalf("remaining timers = %+v, want only tea", list)
	}
	manager.Cancel("")
}

// TestSetLabelExtraction covers the spoken forms the Fast path accepts.
func TestSetLabelExtraction(t *testing.T) {
	reg := timerRegistry(t, timer.NewManager(func() time.Time { return fixedClock() }, nil, nil))
	cases := []struct {
		utterance string
		label     string
	}{
		{"set a pasta timer for 10 minutes", "pasta"},
		{"start a tea timer for five minutes", "tea"},
		{"set a timer for 10 minutes called pasta", "pasta"},
		{"set a timer called tea for five minutes", "tea"},
		{"set an egg timer for 3 minutes", "egg"},
		{"Set a Pasta Timer for ten minutes.", "pasta"},
		{"start a five minute timer", ""},
		{"set a timer for 10 minutes", ""},
	}
	for _, testCase := range cases {
		args, ok := reg.Extract(timer.SetName, testCase.utterance)
		if !ok {
			t.Errorf("%q: Extract failed", testCase.utterance)
			continue
		}
		var in timer.SetIn
		if err := json.Unmarshal(args, &in); err != nil {
			t.Errorf("%q: unmarshal: %v", testCase.utterance, err)
			continue
		}
		if in.Label != testCase.label {
			t.Errorf("%q: label = %q, want %q", testCase.utterance, in.Label, testCase.label)
		}
	}
}

// TestRemainingLabelExtraction covers the lookup and cancel forms.
func TestRemainingLabelExtraction(t *testing.T) {
	reg := timerRegistry(t, timer.NewManager(func() time.Time { return fixedClock() }, nil, nil))
	cases := []struct {
		tool      string
		utterance string
		label     string
	}{
		{timer.RemainingName, "how long is left on the pasta timer", "pasta"},
		{timer.RemainingName, "how much time is left on the tea timer", "tea"},
		{timer.RemainingName, "when is the egg timer done", "egg"},
		{timer.RemainingName, "what's left on the pasta timer", "pasta"},
		{timer.RemainingName, "how long is left on my timer", ""},
		{timer.RemainingName, "how much time is left", ""},
		{timer.CancelName, "cancel the pasta timer", "pasta"},
		{timer.CancelName, "stop my tea timer", "tea"},
		{timer.CancelName, "cancel the timer", ""},
	}
	for _, testCase := range cases {
		args, ok := reg.Extract(testCase.tool, testCase.utterance)
		if !ok {
			t.Errorf("%s %q: Extract failed", testCase.tool, testCase.utterance)
			continue
		}
		var label string
		if testCase.tool == timer.CancelName {
			var in timer.CancelIn
			if err := json.Unmarshal(args, &in); err != nil {
				t.Errorf("%q: unmarshal: %v", testCase.utterance, err)
				continue
			}
			label = in.Label
		} else {
			var in timer.RemainingIn
			if err := json.Unmarshal(args, &in); err != nil {
				t.Errorf("%q: unmarshal: %v", testCase.utterance, err)
				continue
			}
			label = in.Label
		}
		if label != testCase.label {
			t.Errorf("%q: label = %q, want %q", testCase.utterance, label, testCase.label)
		}
	}
}

// TestRemainingSpeaksLabelAndDuration checks the reply wording the eval sees.
func TestRemainingSpeaksLabelAndDuration(t *testing.T) {
	reg, manager := labelRegistry(t)
	manager.Start("pasta", 10*time.Minute)
	args, _ := reg.Extract(timer.RemainingName, "how long is left on the pasta timer")
	result, err := reg.Execute(context.Background(), timer.RemainingName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, _ := reg.Speak(timer.RemainingName, result)
	if !strings.Contains(spoken, "pasta") || !strings.Contains(spoken, "10 minutes") {
		t.Fatalf("Speak = %q, want the label and the duration", spoken)
	}
	manager.Cancel("")
}
