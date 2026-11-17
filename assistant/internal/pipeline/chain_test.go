package pipeline_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Chain callbacks form a synchronous playback barrier between confident Tool calls.
func TestChainRepliesInOrder(t *testing.T) {
	p, runner := newClausePipeline(t)
	p.BindChains(func() bool { return true })
	var replies []string
	reply := p.HandleEvents(context.Background(), request(), "what time is it and set a timer for five minutes", pipeline.Events{StepReply: func(s string) error { replies = append(replies, s); return nil }})
	if reply.Path != tool.PathFast || !reflect.DeepEqual(replies, []string{"clock.now", "timer.set"}) || len(runner.calls()) != 0 {
		t.Fatalf("reply=%+v steps=%v agent=%v", reply, replies, runner.calls())
	}
}

// Ambiguity must cause no partial fast-path execution.
func TestUncertainChainUsesAgent(t *testing.T) {
	p, runner := newClausePipeline(t)
	p.BindChains(func() bool { return true })
	p.HandleEvents(context.Background(), request(), "set a timer for five minutes and tell me a joke", pipeline.Events{StepReply: func(string) error { t.Fatal("partial chain executed"); return nil }})
	if len(runner.calls()) != 1 {
		t.Fatal("expected Agent fallback")
	}
}

// Queue consumption is bounded, cancellable and at most once per completion.
func TestDeferredQueue(t *testing.T) {
	q := &pipeline.DeferredQueue{}
	for i := 0; i < 8; i++ {
		if !q.Add("timer", "what time is it") {
			t.Fatal("early overflow")
		}
	}
	if q.Add("timer", "overflow") {
		t.Fatal("unbounded queue")
	}
	q.Cancel(q.List()[0].ID)
	// Nothing is dispatchable before the completion event, and a wrong event
	// must not release the timer actions either.
	q.Trigger("music")
	if _, ok := q.NextReady(); ok {
		t.Fatal("dispatched before the matching event")
	}
	q.Trigger("timer")
	for i := 0; i < 7; i++ {
		if _, ok := q.NextReady(); !ok {
			t.Fatalf("missing timer action %d", i)
		}
	}
	if _, ok := q.NextReady(); ok {
		t.Fatal("action dispatched more than once")
	}
	q.Add("reply", "test")
	q.Cancel(0)
	if len(q.List()) != 0 {
		t.Fatal("cancel all")
	}
}

// A failed reply barrier aborts the chain before the second Tool.
func TestChainStopsOnPlaybackFailure(t *testing.T) {
	p, _ := newClausePipeline(t)
	p.BindChains(func() bool { return true })
	calls := 0
	p.HandleEvents(context.Background(), request(), "what time is it then set a timer for five minutes", pipeline.Events{StepReply: func(string) error { calls++; return context.Canceled }})
	if calls != 1 {
		t.Fatalf("played %d steps after failure", calls)
	}
}

// Triggered entries remain cancellable while the previous action is executing.
func TestDeferredReadyCancellation(t *testing.T) {
	q := &pipeline.DeferredQueue{}
	q.Add("reply", "first")
	q.Add("reply", "second")
	q.Trigger("reply")
	first, ok := q.NextReady()
	if !ok || first.Text != "first" {
		t.Fatal("missing first action")
	}
	q.Cancel(q.List()[0].ID)
	if _, ok := q.NextReady(); ok {
		t.Fatal("cancelled action executed")
	}
}

// Deferred requests are registered by chat/voice and share the same cancellation state.
func TestDeferredVoiceRegistrationAndCancellation(t *testing.T) {
	p, _ := newClausePipeline(t)
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)
	reply := p.Handle(context.Background(), request(), "after the timer set a timer for five minutes")
	if len(q.List()) != 1 || reply.Path != tool.PathControlPhrase {
		t.Fatalf("not queued: %+v", reply)
	}
	p.Handle(context.Background(), request(), "cancel queued actions")
	if len(q.List()) != 0 {
		t.Fatal("voice cancellation left pending action")
	}
}

// A failing leaf reports its position and prevents every later side effect.
func TestChainToolFailureAborts(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	nextCalls := 0
	for _, name := range []string{"chain.fail", "chain.next"} {
		err := tool.Add(reg, tool.Tool[fakeIn, fakeOut]{Name: name, Description: "test", Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo,
			Handler: func(context.Context, fakeIn) (fakeOut, error) {
				if name == "chain.fail" {
					return fakeOut{}, errors.New("failed")
				}
				nextCalls++
				return fakeOut{Value: "next"}, nil
			},
			Speak: func(out fakeOut) string { return out.Value },
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := pipeline.New(reg, map[string]string{"fail": "chain.fail", "succeed": "chain.next"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.BindChains(func() bool { return true })
	reply := p.Handle(context.Background(), request(), "fail and succeed")
	if nextCalls != 0 || !strings.Contains(reply.Text, "Step 1 failed") {
		t.Fatalf("next=%d reply=%+v", nextCalls, reply)
	}
}

// chainMusicHolder simulates Spotify holding Music Focus.
type chainMusicHolder struct {
	stopped bool
}

// Intents reports the Focus intents the music holder handles.
func (h *chainMusicHolder) Intents() []focus.Intent {
	return []focus.Intent{focus.Stop, focus.Pause}
}

// HandleIntent stops music when the Stop intent is received.
func (h *chainMusicHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	if intent == focus.Stop {
		h.stopped = true
		return "Stopped the music.", true
	}
	return "", false
}

// newChainFocusPipeline sets up a pipeline with lights.power, semantic routing, and optional music focus.
func newChainFocusPipeline(t *testing.T, holder *chainMusicHolder) (*pipeline.Pipeline, *int) {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	lightsCalls := 0
	err := tool.Add(reg, tool.Tool[fakeIn, fakeOut]{
		Name:        "lights.power",
		Description: "Turn lights on or off.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, fakeIn) (fakeOut, error) {
			lightsCalls++
			return fakeOut{Value: "Turned off the living room lights."}, nil
		},
		Speak:    func(out fakeOut) string { return out.Value },
		Examples: []string{"turn off the living room lights"},
	})
	if err != nil {
		t.Fatalf("Add lights.power: %v", err)
	}
	vectors := map[string][]float32{
		"turn off the living room lights": {1, 0},
	}
	embedder := &fakeEmbedder{vectors: vectors}
	router := pipeline.NewSemanticRouter(reg, embedder, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	focusReg := focus.New()
	if holder != nil {
		focusReg.Claim(focus.Music, holder)
	}
	p, err := pipeline.New(reg,
		map[string]string{"stop": "stop"},
		nil,
		pipeline.WithSemanticRouter(router),
		pipeline.WithFocus(focusReg),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	p.BindChains(func() bool { return true })
	return p, &lightsCalls
}

// TestChainFocusIntentSuccess verifies that "stop and turn off the living room lights" stops the music, then runs the lights Tool.
func TestChainFocusIntentSuccess(t *testing.T) {
	music := &chainMusicHolder{}
	p, lightsCalls := newChainFocusPipeline(t, music)
	var steps []string
	events := pipeline.Events{
		StepReply: func(text string) error {
			steps = append(steps, text)
			return nil
		},
	}
	reply := p.HandleEvents(context.Background(), request(), "stop and turn off the living room lights", events)
	if reply.Path != tool.PathFast {
		t.Fatalf("reply.Path = %q, want %q", reply.Path, tool.PathFast)
	}
	if !music.stopped {
		t.Fatal("music was not stopped")
	}
	if *lightsCalls != 1 {
		t.Fatalf("lightsCalls = %d, want 1", *lightsCalls)
	}
	wantSteps := []string{"Stopped the music.", "Turned off the living room lights."}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("steps = %v, want %v", steps, wantSteps)
	}
	wantText := "Stopped the music. Turned off the living room lights."
	if reply.Text != wantText {
		t.Fatalf("reply.Text = %q, want %q", reply.Text, wantText)
	}
}

// TestChainFocusIntentUnheldFails verifies that an intent step nothing holds fails the chain and cancels remaining steps.
func TestChainFocusIntentUnheldFails(t *testing.T) {
	p, lightsCalls := newChainFocusPipeline(t, nil)
	var steps []string
	events := pipeline.Events{
		StepReply: func(text string) error {
			steps = append(steps, text)
			return nil
		},
	}
	reply := p.HandleEvents(context.Background(), request(), "stop and turn off the living room lights", events)
	if *lightsCalls != 0 {
		t.Fatalf("lightsCalls = %d, want 0 (remaining steps cancelled)", *lightsCalls)
	}
	if !strings.Contains(reply.Text, "Step 1 failed") || !strings.Contains(reply.Text, "Remaining steps cancelled.") {
		t.Fatalf("reply.Text = %q, want Step 1 failed cancellation", reply.Text)
	}
}

// TestChainFocusIntentDeferred verifies that a deferred chain with a Focus intent validates and dispatches the same way.
func TestChainFocusIntentDeferred(t *testing.T) {
	music := &chainMusicHolder{}
	p, lightsCalls := newChainFocusPipeline(t, music)
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)
	reply := p.Handle(context.Background(), request(), "after this reply, stop and turn off the living room lights")
	if reply.Path != tool.PathControlPhrase || len(q.List()) != 1 {
		t.Fatalf("reply = %+v, queue len = %d", reply, len(q.List()))
	}
	q.Trigger("reply")
	action, ok := q.NextReady()
	if !ok || action.Text != "stop and turn off the living room lights" {
		t.Fatalf("NextReady = %+v, %v", action, ok)
	}
	var steps []string
	events := pipeline.Events{
		StepReply: func(text string) error {
			steps = append(steps, text)
			return nil
		},
	}
	dispatchedReply := p.HandleEvents(context.Background(), request(), action.Text, events)
	if dispatchedReply.Path != tool.PathFast {
		t.Fatalf("dispatched reply.Path = %q, want %q", dispatchedReply.Path, tool.PathFast)
	}
	if !music.stopped {
		t.Fatal("music was not stopped on deferred dispatch")
	}
	if *lightsCalls != 1 {
		t.Fatalf("lightsCalls = %d, want 1", *lightsCalls)
	}
	wantText := "Stopped the music. Turned off the living room lights."
	if dispatchedReply.Text != wantText {
		t.Fatalf("dispatched reply.Text = %q, want %q", dispatchedReply.Text, wantText)
	}
}

// TestChainConfirmToolDisallowed verifies that a confirm Tool cannot be a step in a chain.
func TestChainConfirmToolDisallowed(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	confirmCalls := 0
	err := tool.Add(reg, tool.Tool[fakeIn, fakeOut]{
		Name:        "chain.confirm",
		Description: "Tool requiring confirm.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, fakeIn) (fakeOut, error) {
			confirmCalls++
			return fakeOut{Value: "done"}, nil
		},
		Speak:    func(out fakeOut) string { return out.Value },
		Examples: []string{"run dangerous action"},
	})
	if err != nil {
		t.Fatal(err)
	}
	vectors := map[string][]float32{
		"run dangerous action": {1, 0},
	}
	embedder := &fakeEmbedder{vectors: vectors}
	router := pipeline.NewSemanticRouter(reg, embedder, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	music := &chainMusicHolder{}
	focusReg := focus.New()
	focusReg.Claim(focus.Music, music)
	p, err := pipeline.New(reg,
		map[string]string{"stop": "stop"},
		nil,
		pipeline.WithSemanticRouter(router),
		pipeline.WithFocus(focusReg),
	)
	if err != nil {
		t.Fatal(err)
	}
	p.BindChains(func() bool { return true })
	reply := p.Handle(context.Background(), request(), "stop and run dangerous action")
	if reply.Path == tool.PathFast || confirmCalls != 0 || music.stopped {
		t.Fatalf("confirm tool ran in chain: reply=%+v, calls=%d, stopped=%v", reply, confirmCalls, music.stopped)
	}
}
