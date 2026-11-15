package timer_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// discardLogger keeps an alert test's log lines out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitUntil polls condition until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestListAndRemainingReportTheRingingTimer is the regression for #197: while
// the spoken alert blocks, both read Tools must still report the Timer the user
// can hear, marked ringing, and drop it only once the alert has ended.
func TestListAndRemainingReportTheRingingTimer(t *testing.T) {
	alert := newBlockingAlert()
	manager := timer.NewManager(nil, alert.SayText, discardLogger())
	defer manager.Close()

	started, err := manager.Start("pasta", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForAlert(t, alert)

	list := manager.List()
	if len(list) != 1 {
		t.Fatalf("List while ringing = %+v, want the ringing Timer", list)
	}
	ringing := list[0]
	if ringing.ID != started.ID || ringing.Label != "pasta" {
		t.Fatalf("List while ringing = %+v, want the pasta Timer", ringing)
	}
	if ringing.State != timer.StateRinging {
		t.Fatalf("state = %q, want %q", ringing.State, timer.StateRinging)
	}
	if ringing.RemainingSeconds != 0 {
		t.Fatalf("remaining = %d, want 0 while ringing", ringing.RemainingSeconds)
	}

	for _, label := range []string{"pasta", ""} {
		got, ok := manager.Remaining(label)
		if !ok {
			t.Fatalf("Remaining(%q) found nothing while the alert was ringing", label)
		}
		if got.ID != started.ID || got.State != timer.StateRinging {
			t.Fatalf("Remaining(%q) = %+v, want the ringing pasta Timer", label, got)
		}
	}
	if _, ok := manager.Remaining("tea"); ok {
		t.Fatal("Remaining(tea) matched the ringing pasta Timer")
	}

	// Ending the alert releases the ringing marker as well as the pending row.
	manager.Cancel("")
	waitUntil(t, "the ringing Timer to leave List", func() bool { return len(manager.List()) == 0 })
	if got, ok := manager.Remaining("pasta"); ok {
		t.Fatalf("Remaining(pasta) = %+v after the alert ended, want nothing", got)
	}
}

// TestTimerToolsSpeakTheRingingAlert covers the spoken wording at the Tool
// seam: a single ringing Timer, and a mix of one ringing and one pending.
func TestTimerToolsSpeakTheRingingAlert(t *testing.T) {
	alert := newBlockingAlert()
	manager := timer.NewManager(nil, alert.SayText, discardLogger())
	defer manager.Close()
	reg := tool.NewRegistry(time.Second)
	if err := timer.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := manager.Start("tea", 10*time.Minute); err != nil {
		t.Fatalf("Start tea: %v", err)
	}
	ringing, err := manager.Start("pasta", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Start pasta: %v", err)
	}
	waitForAlert(t, alert)

	result, err := reg.Execute(context.Background(), timer.ListName, nil)
	if err != nil {
		t.Fatalf("timer.list: %v", err)
	}
	list, ok := result.(timer.ListOut)
	if !ok {
		t.Fatalf("timer.list returned %T", result)
	}
	if len(list.Timers) != 2 {
		t.Fatalf("timers = %+v, want the ringing and the pending one", list.Timers)
	}
	states := make(map[string]string, len(list.Timers))
	for _, item := range list.Timers {
		states[item.Label] = item.State
	}
	if states["pasta"] != timer.StateRinging || states["tea"] != timer.StatePending {
		t.Fatalf("states = %v, want pasta ringing and tea pending", states)
	}
	if list.Timers[0].Label != "pasta" {
		t.Fatalf("timers = %+v, want the ringing Timer first", list.Timers)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	if !strings.Contains(string(raw), `"state":"ringing"`) {
		t.Fatalf("list JSON = %s, want a ringing state", raw)
	}
	spoken, _ := reg.Speak(timer.ListName, result)
	if want := "You have 2 timers. The pasta timer is ringing."; spoken != want {
		t.Fatalf("list Speak = %q, want %q", spoken, want)
	}

	args, ok := reg.Extract(timer.RemainingName, "how long is left on the pasta timer")
	if !ok {
		t.Fatal("remaining Extract failed")
	}
	result, err = reg.Execute(context.Background(), timer.RemainingName, args)
	if err != nil {
		t.Fatalf("timer.remaining: %v", err)
	}
	remaining, ok := result.(timer.RemainingOut)
	if !ok || remaining.Timer.State != timer.StateRinging {
		t.Fatalf("remaining = %+v, want the ringing pasta Timer", result)
	}
	spoken, _ = reg.Speak(timer.RemainingName, result)
	if want := "Your pasta timer is ringing."; spoken != want {
		t.Fatalf("remaining Speak = %q, want %q", spoken, want)
	}

	// The pending Timer keeps its previous answer and ordering behaviour.
	args, ok = reg.Extract(timer.RemainingName, "how long is left on the tea timer")
	if !ok {
		t.Fatal("tea remaining Extract failed")
	}
	result, err = reg.Execute(context.Background(), timer.RemainingName, args)
	if err != nil {
		t.Fatalf("tea timer.remaining: %v", err)
	}
	remaining = result.(timer.RemainingOut)
	if remaining.Timer.State != timer.StatePending {
		t.Fatalf("tea state = %q, want %q", remaining.Timer.State, timer.StatePending)
	}
	spoken, _ = reg.Speak(timer.RemainingName, result)
	if want := "The tea timer has 10 minutes left."; spoken != want {
		t.Fatalf("tea remaining Speak = %q, want %q", spoken, want)
	}

	manager.Cancel(ringing.ID)
	waitUntil(t, "only the pending tea Timer to remain", func() bool { return len(manager.List()) == 1 })
	if got := manager.List()[0]; got.Label != "tea" || got.State != timer.StatePending {
		t.Fatalf("after the alert ended = %+v, want the pending tea Timer", got)
	}
}

// TestPendingTimersKeepTheirSpokenAnswer pins the pre-existing pending-only
// wording and the default state, so the ringing metadata does not shift it.
func TestPendingTimersKeepTheirSpokenAnswer(t *testing.T) {
	manager := timer.NewManager(nil, nil, discardLogger())
	defer manager.Close()
	reg := timerRegistry(t, manager)

	started, err := manager.Start("pasta", 10*time.Minute)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.State != timer.StatePending {
		t.Fatalf("Start state = %q, want %q", started.State, timer.StatePending)
	}
	list := manager.List()
	if len(list) != 1 || list[0].State != timer.StatePending {
		t.Fatalf("List = %+v, want one pending Timer", list)
	}
	args, ok := reg.Extract(timer.RemainingName, "how long is left on the pasta timer")
	if !ok {
		t.Fatal("Extract failed")
	}
	result, err := reg.Execute(context.Background(), timer.RemainingName, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	spoken, _ := reg.Speak(timer.RemainingName, result)
	if want := "The pasta timer has 10 minutes left."; spoken != want {
		t.Fatalf("Speak = %q, want %q", spoken, want)
	}
	manager.Cancel("")
}

// TestRingEngineReportsTheRingingTimer covers the production seam: the timer
// becomes visible in List between the engine's OnStart and the end of its
// Ring, not merely on the fallback speech path.
func TestRingEngineReportsTheRingingTimer(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(true)
	engine := testEngine(registry, player, ring.Settings{}, nil, nil)
	manager := managerWithStore(t, st, registry, engine)

	started, err := manager.Start("pasta", 2*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-player.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the Ring never started")
	}
	list := manager.List()
	if len(list) != 1 || list[0].ID != started.ID || list[0].State != timer.StateRinging {
		t.Fatalf("List while the Ring plays = %+v, want the ringing pasta Timer", list)
	}

	manager.Cancel(started.ID)
	waitUntil(t, "the Ring's Timer to leave List", func() bool { return len(manager.List()) == 0 })
}

// TestQueuedTimerIsNotReportedAsRinging pins the other half of the marker: a
// Timer that has fired but is still waiting behind another Ring is not claimed
// as ringing until its own Ring takes the one speaker slot.
func TestQueuedTimerIsNotReportedAsRinging(t *testing.T) {
	st := openTimerStore(t, t.TempDir()+"/data/assistant.db")
	t.Cleanup(func() { _ = st.Close() })
	registry := focus.New()
	player := newTestPlayer(true)
	engine := testEngine(registry, player, ring.Settings{}, nil, nil)
	manager := managerWithStore(t, st, registry, engine)

	first, err := manager.Start("pasta", 2*time.Millisecond)
	if err != nil {
		t.Fatalf("Start pasta: %v", err)
	}
	select {
	case <-player.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the first Ring never started")
	}

	// The second Timer fires while pasta owns the speaker, so it holds an open
	// history row but no Ring of its own yet.
	second, err := manager.Start("tea", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Start tea: %v", err)
	}
	waitUntil(t, "the queued Timer to fire", func() bool {
		events, err := manager.History(10)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "timer" && event.SourceID == second.ID && event.State.Open() {
				return true
			}
		}
		return false
	})
	if list := manager.List(); len(list) != 1 || list[0].ID != first.ID || list[0].State != timer.StateRinging {
		t.Fatalf("List while tea waits behind the Ring = %+v, want only the ringing pasta Timer", list)
	}

	// Ending pasta's Ring lets tea start, and only then does it ring.
	manager.Cancel(first.ID)
	waitUntil(t, "the queued Timer to become the ringing one", func() bool {
		list := manager.List()
		return len(list) == 1 && list[0].ID == second.ID && list[0].State == timer.StateRinging
	})
	manager.Cancel(second.ID)
	waitUntil(t, "both Timers to leave List", func() bool { return len(manager.List()) == 0 })
}

// TestRingingTimerStaysOffTheAgentPath proves the privacy half of #197: the
// ringing Timer's private label is visible to the Local-only read Tools on the
// Pi, but the Agent path is neither offered those Tools nor given a Local-only
// turn that carried the label.
func TestRingingTimerStaysOffTheAgentPath(t *testing.T) {
	const privateLabel = "divorce lawyer"
	alert := newBlockingAlert()
	manager := timer.NewManager(nil, alert.SayText, discardLogger())
	defer manager.Close()
	reg := timerRegistry(t, manager)

	if _, err := manager.Start(privateLabel, 10*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForAlert(t, alert)

	// The owner-facing read path does show what is ringing.
	list := manager.List()
	if len(list) != 1 || list[0].Label != privateLabel || list[0].State != timer.StateRinging {
		t.Fatalf("List while ringing = %+v, want the private ringing Timer", list)
	}

	// The Agent path is not offered timer.list or timer.remaining at all, so
	// the model cannot ask for the label while the alert sounds.
	offered := agent.NewPromptBuilder("").Tools(reg)
	for _, def := range offered {
		if def.OfFunction == nil {
			continue
		}
		switch def.OfFunction.Function.Name {
		case "timer__list", "timer__remaining":
			t.Errorf("Agent path was offered %s", def.OfFunction.Function.Name)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("marshal offered Tool: %v", err)
		}
		if strings.Contains(string(raw), privateLabel) {
			t.Errorf("an offered Tool definition carried the private label: %s", raw)
		}
	}

	// Even a recorded Local-only turn that held the label is dropped whole
	// before the prompt is built.
	messages := agent.NewPromptBuilder("").Messages("what's ringing?", []agent.Turn{{
		Utterance: "what timers do I have",
		Reply:     "The " + privateLabel + " timer is ringing.",
		Calls: []agent.ToolCall{{
			Name:      timer.ListName,
			Result:    json.RawMessage(`{"timers":[{"label":"` + privateLabel + `","state":"ringing"}]}`),
			LocalOnly: true,
		}},
	}})
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal prompt messages: %v", err)
	}
	if strings.Contains(string(raw), privateLabel) {
		t.Fatalf("the Agent prompt carried a Local-only ringing label: %s", raw)
	}

	manager.Cancel("")
	waitUntil(t, "the ringing Timer to leave List", func() bool { return len(manager.List()) == 0 })
}
