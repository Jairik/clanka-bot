package alarm_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// holdingSay blocks the spoken fallback until the test releases it, so an
// Alarm stays mid-Ring while the test reads List.
type holdingSay struct {
	started chan struct{}
	texts   chan string
	release chan struct{}
	once    sync.Once
}

func newHoldingSay() *holdingSay {
	return &holdingSay{
		started: make(chan struct{}, 4),
		texts:   make(chan string, 4),
		release: make(chan struct{}),
	}
}

// releaseOnce ends the hold, and is safe to call from a cleanup after a test
// has already released the speaker.
func (s *holdingSay) releaseOnce() { s.once.Do(func() { close(s.release) }) }

func (s *holdingSay) SayText(ctx context.Context, text string) error {
	select {
	case s.texts <- text:
	default:
	}
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

func (s *holdingSay) waitStart(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the Alarm never started speaking")
	}
}

// waitAlarmList polls until List reports want rows, so a released Ring's
// asynchronous unwind cannot race the assertion.
func waitAlarmList(t *testing.T, manager *alarm.Manager, want int) []alarm.Alarm {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		list := manager.List()
		if len(list) == want {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("List = %+v, want %d rows", list, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newRingingRegistry builds the real alarm module over one Manager.
func newRingingRegistry(t *testing.T, manager *alarm.Manager) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(5 * time.Second)
	if err := alarm.Register(reg, manager); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// stateByLabel indexes the listed Alarms by label.
func stateByLabel(alarms []alarm.Alarm) map[string]alarm.Alarm {
	out := make(map[string]alarm.Alarm, len(alarms))
	for _, item := range alarms {
		out[item.Label] = item
	}
	return out
}

// TestListReportsTheRingingAlarm is the regression for #197's Alarm half: while
// the spoken fallback blocks, alarm.list must include the Alarm the user can
// hear, marked ringing with the occurrence that is sounding, and drop it only
// once the alert has ended.
func TestListReportsTheRingingAlarm(t *testing.T) {
	say := newHoldingSay()
	t.Cleanup(say.releaseOnce)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	manager := newManager(t, clock, say.SayText, nil, nil)
	reg := newRingingRegistry(t, manager)

	created, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Label: "wake"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if created.State != alarm.StatePending {
		t.Fatalf("Set state = %q, want %q", created.State, alarm.StatePending)
	}
	say.waitStart(t)

	list := manager.List()
	if len(list) != 1 {
		t.Fatalf("List while ringing = %+v, want the ringing Alarm", list)
	}
	ringing := list[0]
	if ringing.ID != created.ID || ringing.Label != "wake" {
		t.Fatalf("List while ringing = %+v, want the wake Alarm", ringing)
	}
	if ringing.State != alarm.StateRinging {
		t.Fatalf("state = %q, want %q", ringing.State, alarm.StateRinging)
	}
	// A ringing Alarm reports the occurrence the user hears, not a later one.
	if want := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC); !ringing.NextFiresAt.Equal(want) {
		t.Fatalf("next = %s, want the ringing occurrence %s", ringing.NextFiresAt, want)
	}

	// The Tool seam answers about the alert the user hears, and the JSON
	// carries the marker the Dashboard reads.
	result, err := reg.Execute(context.Background(), alarm.ListName, nil)
	if err != nil {
		t.Fatalf("alarm.list: %v", err)
	}
	out, ok := result.(alarm.ListOut)
	if !ok || len(out.Alarms) != 1 || out.Alarms[0].State != alarm.StateRinging {
		t.Fatalf("alarm.list = %+v, want the ringing Alarm", result)
	}
	if spoken, _ := reg.Speak(alarm.ListName, result); spoken != "Your wake alarm is ringing." {
		t.Fatalf("list Speak = %q, want the ringing wording", spoken)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	if !strings.Contains(string(raw), `"state":"ringing"`) {
		t.Fatalf("list JSON = %s, want a ringing state", raw)
	}

	// Ending the alert releases the ringing marker as well as the pending row.
	say.releaseOnce()
	waitAlarmList(t, manager, 0)
}

// TestAlarmToolsSpeakTheRingingAlarm covers the spoken wording with a mix of
// one ringing and one pending Alarm, and the pending-only answer afterwards.
func TestAlarmToolsSpeakTheRingingAlarm(t *testing.T) {
	say := newHoldingSay()
	t.Cleanup(say.releaseOnce)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	manager := newManager(t, clock, say.SayText, nil, nil)
	reg := newRingingRegistry(t, manager)

	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 8, Label: "tea"}); err != nil {
		t.Fatalf("Set tea: %v", err)
	}
	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Label: "wake"}); err != nil {
		t.Fatalf("Set wake: %v", err)
	}
	say.waitStart(t)

	result, err := reg.Execute(context.Background(), alarm.ListName, nil)
	if err != nil {
		t.Fatalf("alarm.list: %v", err)
	}
	out, ok := result.(alarm.ListOut)
	if !ok || len(out.Alarms) != 2 {
		t.Fatalf("alarm.list = %+v, want the ringing and the pending Alarm", result)
	}
	states := stateByLabel(out.Alarms)
	if got := states["wake"].State; got != alarm.StateRinging {
		t.Fatalf("wake state = %q, want %q", got, alarm.StateRinging)
	}
	if got := states["tea"].State; got != alarm.StatePending {
		t.Fatalf("tea state = %q, want %q", got, alarm.StatePending)
	}
	if spoken, _ := reg.Speak(alarm.ListName, result); spoken != "You have 2 alarms. The wake alarm is ringing." {
		t.Fatalf("list Speak = %q, want the ringing wording", spoken)
	}

	// The pending Alarm keeps its previous answer once the Ring has ended.
	say.releaseOnce()
	waitAlarmList(t, manager, 1)
	result, err = reg.Execute(context.Background(), alarm.ListName, nil)
	if err != nil {
		t.Fatalf("alarm.list after the Ring: %v", err)
	}
	out = result.(alarm.ListOut)
	if len(out.Alarms) != 1 || out.Alarms[0].ID != states["tea"].ID ||
		out.Alarms[0].State != alarm.StatePending {
		t.Fatalf("alarm.list after the Ring = %+v, want the pending tea Alarm", out.Alarms)
	}
	if spoken, _ := reg.Speak(alarm.ListName, result); spoken != "Tea alarm is set for 8:00 am." {
		t.Fatalf("list Speak = %q, want the pending wording", spoken)
	}
}

// TestPendingAlarmsKeepTheirSpokenAnswer pins the pre-existing pending-only
// wording and the default state, so the ringing metadata does not shift it.
func TestPendingAlarmsKeepTheirSpokenAnswer(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	manager := newManager(t, clock, nil, nil, nil)
	reg := newRingingRegistry(t, manager)

	created, err := manager.Set(context.Background(), alarm.SetIn{Hour: 8, Schedule: "daily", Label: "medication"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if created.State != alarm.StatePending {
		t.Fatalf("Set state = %q, want %q", created.State, alarm.StatePending)
	}
	list := manager.List()
	if len(list) != 1 || list[0].State != alarm.StatePending || !list[0].NextFiresAt.Equal(created.NextFiresAt) {
		t.Fatalf("List = %+v, want one pending Alarm at %s", list, created.NextFiresAt)
	}
	result, err := reg.Execute(context.Background(), alarm.ListName, nil)
	if err != nil {
		t.Fatalf("alarm.list: %v", err)
	}
	if spoken, _ := reg.Speak(alarm.ListName, result); spoken != "Medication alarm is set for 8:00 am every day." {
		t.Fatalf("list Speak = %q, want the pending wording", spoken)
	}
	if raw, err := json.Marshal(result); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if strings.Contains(string(raw), `"state":"ringing"`) {
		t.Fatalf("pending list JSON = %s, want no ringing state", raw)
	}
	if got := manager.CancelAll(); got != 1 {
		t.Fatalf("CancelAll = %d, want 1", got)
	}
}

// TestRingEngineReportsTheRingingAlarm covers the production seam: the Alarm
// becomes visible in List between the engine's OnStart and the end of its Ring,
// with the occurrence that is sounding rather than the repeat's next one.
func TestRingEngineReportsTheRingingAlarm(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	focusRegistry := focus.New()
	player := newRingPlayer(func(_ int, ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	manager := newManager(t, clock, nil, nil, nil)
	manager.SetRingEngine(testEngine(focusRegistry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	created, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily", Label: "wake"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case <-player.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the Alarm never rang")
	}

	list := manager.List()
	if len(list) != 1 || list[0].ID != created.ID || list[0].State != alarm.StateRinging {
		t.Fatalf("List while the Ring plays = %+v, want the ringing wake Alarm", list)
	}
	if want := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC); !list[0].NextFiresAt.Equal(want) {
		t.Fatalf("next = %s, want the ringing occurrence %s, not tomorrow", list[0].NextFiresAt, want)
	}

	// Stopping the Ring keeps the daily Alarm and restores its next occurrence.
	if text, handled := focusRegistry.HandleIntent(context.Background(), focus.Stop); !handled || text != "Alarm stopped." {
		t.Fatalf("stop = %q, %v; want Alarm stopped", text, handled)
	}
	waitForAlarmState(t, manager, alarm.StatePending)
	list = manager.List()
	wantNext := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	if len(list) != 1 || list[0].State != alarm.StatePending || !list[0].NextFiresAt.Equal(wantNext) {
		t.Fatalf("List after the Ring = %+v, want one pending Alarm at %s", list, wantNext)
	}
}

// waitForAlarmState polls until the single listed Alarm reports want.
func waitForAlarmState(t *testing.T, manager *alarm.Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		list := manager.List()
		if len(list) == 1 && list[0].State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("List = %+v, want one Alarm in state %q", list, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestQueuedAlarmIsNotReportedAsRinging pins the other half of the marker: an
// Alarm that has fired but is still waiting behind another Ring is not claimed
// as ringing until its own Ring takes the one speaker slot.
func TestQueuedAlarmIsNotReportedAsRinging(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	focusRegistry := focus.New()
	player := newRingPlayer(func(_ int, ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	manager := newManager(t, clock, nil, nil, nil)
	manager.SetRingEngine(testEngine(focusRegistry, player))
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	first, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily", Label: "wake"})
	if err != nil {
		t.Fatalf("Set wake: %v", err)
	}
	select {
	case <-player.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the first Alarm never rang")
	}

	// The second Alarm fires while wake owns the speaker, so it holds an open
	// history row but no Ring of its own yet. Moving the clock forward 50 ms
	// keeps its own delay shorter than the first Alarm's 100 ms.
	clock.Set(time.Date(2026, 11, 12, 6, 59, 59, 950_000_000, time.UTC))
	second, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Schedule: "daily", Label: "gym"})
	if err != nil {
		t.Fatalf("Set gym: %v", err)
	}
	waitAlarmEvent(t, manager, second.ID, true)

	list := manager.List()
	if len(list) != 2 {
		t.Fatalf("List while gym waits behind the Ring = %+v, want wake ringing and gym pending", list)
	}
	states := stateByLabel(list)
	if got := states["wake"].State; got != alarm.StateRinging {
		t.Fatalf("wake state = %q, want %q", got, alarm.StateRinging)
	}
	if got := states["gym"].State; got != alarm.StatePending {
		t.Fatalf("gym state = %q, want %q: a queued Alarm is not ringing", got, alarm.StatePending)
	}

	// Ending wake's Ring lets gym start, and only then does it ring.
	if _, handled := focusRegistry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop intent was not handled")
	}
	waitForRingingAlarm(t, manager, second.ID)
	if got := manager.List(); len(got) != 2 {
		t.Fatalf("List once gym rings = %+v, want wake pending and gym ringing", got)
	}

	if _, handled := focusRegistry.HandleIntent(context.Background(), focus.Stop); !handled {
		t.Fatal("stop intent was not handled")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		list := manager.List()
		ringing := false
		for _, item := range list {
			if item.State == alarm.StateRinging {
				ringing = true
			}
		}
		if len(list) == 2 && !ringing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("List after both Rings = %+v, want two pending Alarms", list)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if first.ID == second.ID {
		t.Fatal("the two Alarms share an id")
	}
}

// waitForRingingAlarm polls until id is the only Alarm reported as ringing.
func waitForRingingAlarm(t *testing.T, manager *alarm.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ringing := 0
		match := false
		for _, item := range manager.List() {
			if item.State != alarm.StateRinging {
				continue
			}
			ringing++
			match = item.ID == id
		}
		if ringing == 1 && match {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("List = %+v, want only %s ringing", manager.List(), id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitAlarmEvent polls the history for one event of id in the wanted open or
// closed state.
func waitAlarmEvent(t *testing.T, manager *alarm.Manager, id string, open bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := manager.History(20)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		for _, event := range events {
			if event.Kind == "alarm" && event.SourceID == id && event.State.Open() == open {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s alarm event for %s in %+v", map[bool]string{true: "open", false: "closed"}[open], id, events)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRingingAlarmStaysOffTheAgentPath proves the privacy half of #197 for the
// Alarm kind: the ringing Alarm's private label is visible to the Local-only
// read Tool on the Pi, but the Agent path is neither offered alarm.list nor
// given a Local-only turn that carried the label.
func TestRingingAlarmStaysOffTheAgentPath(t *testing.T) {
	const privateLabel = "divorce lawyer"
	say := newHoldingSay()
	t.Cleanup(say.releaseOnce)
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	manager := newManager(t, clock, say.SayText, nil, nil)
	reg := newRingingRegistry(t, manager)

	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Label: privateLabel}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	say.waitStart(t)

	// The owner-facing read path does show what is ringing.
	list := manager.List()
	if len(list) != 1 || list[0].Label != privateLabel || list[0].State != alarm.StateRinging {
		t.Fatalf("List while ringing = %+v, want the private ringing Alarm", list)
	}

	info, ok := reg.Info(alarm.ListName)
	if !ok || !info.LocalOnly {
		t.Fatalf("alarm.list LocalOnly = %v, want LocalOnlyYes", info.LocalOnly)
	}
	// The Agent path is not offered alarm.list at all, so the model cannot ask
	// for the label while the alert sounds. The Alarm kind has no remaining
	// Tool to hide beside it.
	offered := agent.NewPromptBuilder("").Tools(reg)
	for _, def := range offered {
		if def.OfFunction == nil {
			continue
		}
		if def.OfFunction.Function.Name == info.WireName {
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
		Utterance: "what alarms do I have",
		Reply:     "Your " + privateLabel + " alarm is ringing.",
		Calls: []agent.ToolCall{{
			Name:      alarm.ListName,
			Result:    json.RawMessage(`{"alarms":[{"label":"` + privateLabel + `","state":"ringing"}]}`),
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

	say.releaseOnce()
	waitAlarmList(t, manager, 0)
}

// TestRingingAlarmKeepsItsOneOffHistory is the existing-behaviour regression:
// a one-off Alarm still deletes its row after the Ring, so a ringing row must
// not leave the Alarm behind in List.
func TestRingingAlarmKeepsItsOneOffHistory(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "data", "assistant.db"))
	defer st.Close()
	clock := &fixedClock{now: time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)}
	say := newHoldingSay()
	t.Cleanup(say.releaseOnce)
	manager := newManager(t, clock, say.SayText, nil, nil)
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}

	if _, err := manager.Set(context.Background(), alarm.SetIn{Hour: 7, Label: "wake"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	say.waitStart(t)
	if list := manager.List(); len(list) != 1 || list[0].State != alarm.StateRinging {
		t.Fatalf("List while ringing = %+v, want the one-off ringing Alarm", list)
	}

	say.releaseOnce()
	waitAlarmList(t, manager, 0)
	events := waitAlarmHistory(t, manager, 1)
	if events[0].State != alertstore.StateStopped || events[0].EndedBy != "spoken" {
		t.Fatalf("event = %+v, want stopped/spoken", events[0])
	}
	waitAlarmRows(t, st, 0)
}
