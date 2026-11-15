package reminder_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// waitListLen polls until List reports want rows or the deadline passes, so a
// released alert's asynchronous unwind cannot race the assertion.
func waitListLen(t *testing.T, manager *reminder.Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(manager.List()) != want {
		if time.Now().After(deadline) {
			t.Fatalf("List = %+v, want %d rows", manager.List(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// holdingSpeaker blocks one spoken line until the test releases it, so a
// Reminder stays mid-alert while the test reads List.
type holdingSpeaker struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHoldingSpeaker() *holdingSpeaker {
	return &holdingSpeaker{started: make(chan struct{}, 4), release: make(chan struct{})}
}

// releaseOnce ends the hold, and is safe to call from a cleanup after a test
// has already released the speaker.
func (s *holdingSpeaker) releaseOnce() { s.once.Do(func() { close(s.release) }) }

func (s *holdingSpeaker) SayText(ctx context.Context, _ string) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *holdingSpeaker) waitStart(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the Reminder never started speaking")
	}
}

// TestListReportsTheRingingReminder is the regression for #197's Reminder
// half: while the message plays, reminder.list must include the Reminder the
// user can hear, marked ringing, and drop it only once playback has ended.
func TestListReportsTheRingingReminder(t *testing.T) {
	const message = "check the oven"
	speaker := newHoldingSpeaker()
	t.Cleanup(speaker.releaseOnce)
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: speaker, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)
	registry := tool.NewRegistry(time.Second)
	if err := reminder.Register(registry, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}

	started, err := manager.StartDuration(message, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if started.State != reminder.StatePending {
		t.Fatalf("StartDuration state = %q, want %q", started.State, reminder.StatePending)
	}
	speaker.waitStart(t)

	list := manager.List()
	if len(list) != 1 {
		t.Fatalf("List while ringing = %+v, want the ringing Reminder", list)
	}
	ringing := list[0]
	if ringing.ID != started.ID || ringing.Message != message {
		t.Fatalf("List while ringing = %+v, want the %q Reminder", ringing, message)
	}
	if ringing.State != reminder.StateRinging {
		t.Fatalf("state = %q, want %q", ringing.State, reminder.StateRinging)
	}
	if ringing.RemainingSeconds != 0 {
		t.Fatalf("remaining = %d, want 0 while ringing", ringing.RemainingSeconds)
	}

	// The Tool seam answers about the alert the user hears, and the JSON
	// carries the marker the Dashboard will read.
	result, err := registry.Execute(context.Background(), reminder.ListName, nil)
	if err != nil {
		t.Fatalf("reminder.list: %v", err)
	}
	out, ok := result.(reminder.ListOut)
	if !ok || len(out.Reminders) != 1 || out.Reminders[0].State != reminder.StateRinging {
		t.Fatalf("reminder.list = %+v, want the ringing Reminder", result)
	}
	if spoken, _ := registry.Speak(reminder.ListName, result); spoken != "Your reminder to "+message+" is ringing." {
		t.Fatalf("list Speak = %q, want the ringing wording", spoken)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	if !strings.Contains(string(raw), `"state":"ringing"`) {
		t.Fatalf("list JSON = %s, want a ringing state", raw)
	}

	// reminder.list stays Local-only, so the private message never reaches the
	// Agent path while it rings.
	info, ok := registry.Info(reminder.ListName)
	if !info.LocalOnly {
		t.Fatalf("reminder.list LocalOnly = %v, want LocalOnlyYes", info.LocalOnly)
	}
	for _, def := range agent.NewPromptBuilder("").Tools(registry) {
		if def.OfFunction == nil {
			continue
		}
		switch def.OfFunction.Function.Name {
		case "reminder__list":
			t.Fatalf("Agent path was offered %s", def.OfFunction.Function.Name)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("marshal offered Tool: %v", err)
		}
		if strings.Contains(string(raw), message) {
			t.Fatalf("an offered Tool definition carried the private message: %s", raw)
		}
	}
	messages := agent.NewPromptBuilder("").Messages("what's ringing?", []agent.Turn{{
		Utterance: "what reminders do I have",
		Reply:     "Your reminder to " + message + " is ringing.",
		Calls: []agent.ToolCall{{
			Name:      reminder.ListName,
			Result:    json.RawMessage(`{"reminders":[{"message":"` + message + `","state":"ringing"}]}`),
			LocalOnly: true,
		}},
	}})
	raw, err = json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal prompt messages: %v", err)
	}
	if strings.Contains(string(raw), message) {
		t.Fatalf("the Agent prompt carried a Local-only ringing message: %s", raw)
	}

	// Ending the alert drops the ringing row again.
	speaker.releaseOnce()
	waitListLen(t, manager, 0)
}

// TestQueuedReminderIsNotReportedAsRinging pins the other half of the marker:
// a Reminder that has fired but whose item still waits behind another alert is
// not claimed as ringing until its own playback starts.
func TestQueuedReminderIsNotReportedAsRinging(t *testing.T) {
	speaker := newHoldingSpeaker()
	t.Cleanup(speaker.releaseOnce)
	queue := announce.New(speaker, testLogger())
	t.Cleanup(queue.Close)
	manager := reminder.NewManager(reminder.Options{Speaker: speaker, Queue: queue, Logger: testLogger()})
	t.Cleanup(manager.Close)

	// One announcement already owns the speaker, so the Reminder's Cue and
	// message wait their turn.
	blocking := make(chan struct{})
	hold := make(chan struct{})
	var holdOnce sync.Once
	releaseHold := func() { holdOnce.Do(func() { close(hold) }) }
	// A failing assertion must not leave the queue worker waiting forever.
	t.Cleanup(releaseHold)
	if !queue.EnqueuePlay(func(ctx context.Context) error {
		close(blocking)
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return nil
	}) {
		t.Fatal("EnqueuePlay rejected the holding item")
	}
	<-blocking

	started, err := manager.StartDuration("check the oven", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	// Its pending row is gone, so it has fired and its item is queued, but it
	// is not the alert the user hears yet.
	waitListLen(t, manager, 0)
	time.Sleep(50 * time.Millisecond)
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("a Reminder queued behind another alert was reported as %+v, want nothing", list)
	}

	releaseHold()
	speaker.waitStart(t)
	list := manager.List()
	if len(list) != 1 || list[0].ID != started.ID || list[0].State != reminder.StateRinging {
		t.Fatalf("List once the queued Reminder plays = %+v, want the ringing Reminder", list)
	}

	speaker.releaseOnce()
	waitListLen(t, manager, 0)
}
