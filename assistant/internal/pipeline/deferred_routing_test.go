package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// deferredRoutingPipeline queues "turn off the lights" as a safe leaf and
// sends unresolved actions to the Agent. Each routing case starts from a fresh
// queue and Agent runner so a case cannot leak state into another.
func deferredRoutingPipeline(t *testing.T) (*pipeline.Pipeline, *pipeline.DeferredQueue, *fakeRunner) {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	addCountingTool(t, reg, "lights.off", &callCounter{})
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	logger, _ := testLogger()
	p, err := pipeline.New(reg, map[string]string{"turn off the lights": "lights.off"}, logger,
		pipeline.WithAgent(runner, nil))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)
	return p, q, runner
}

// TestDeferredRouting covers the deferred-action routing table from #285: the
// trigger phrases, the safe-leaf validation, the Agent fallback, the capacity
// limit, cancellation and the Trigger-gated insertion-order dispatch.
func TestDeferredRouting(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "after the timer queues one action with the right event and text",
			run: func(t *testing.T) {
				p, q, runner := deferredRoutingPipeline(t)
				reply := p.Handle(context.Background(), request(), "after the timer, turn off the lights")
				if reply.Path != tool.PathControlPhrase {
					t.Fatalf("path = %q, want %q (reply %q)", reply.Path, tool.PathControlPhrase, reply.Text)
				}
				queued := q.List()
				if len(queued) != 1 || queued[0].Event != "timer" || queued[0].Text != "turn off the lights" {
					t.Fatalf("queued = %+v, want one timer action for turn off the lights", queued)
				}
				if len(runner.calls()) != 0 {
					t.Fatalf("Agent saw %v, want no fallback for a safe action", runner.calls())
				}
			},
		},
		{
			name: "when the music stops queues a music action",
			run: func(t *testing.T) {
				p, q, _ := deferredRoutingPipeline(t)
				reply := p.Handle(context.Background(), request(), "when the music stops, turn off the lights")
				if reply.Path != tool.PathControlPhrase {
					t.Fatalf("path = %q, want %q (reply %q)", reply.Path, tool.PathControlPhrase, reply.Text)
				}
				queued := q.List()
				if len(queued) != 1 || queued[0].Event != "music" || queued[0].Text != "turn off the lights" {
					t.Fatalf("queued = %+v, want one music action for turn off the lights", queued)
				}
			},
		},
		{
			name: "unresolvable action falls back to the Agent with nothing queued",
			run: func(t *testing.T) {
				p, q, runner := deferredRoutingPipeline(t)
				utterance := "when the music stops, tell me a joke"
				reply := p.Handle(context.Background(), request(), utterance)
				if reply.Path != tool.PathAgent {
					t.Fatalf("path = %q, want %q (reply %q)", reply.Path, tool.PathAgent, reply.Text)
				}
				if reply.Text != "Agent answer." {
					t.Errorf("reply = %q, want the Agent answer", reply.Text)
				}
				if calls := runner.calls(); len(calls) != 1 || calls[0] != utterance {
					t.Fatalf("Agent saw %v, want one call with the whole transcript", calls)
				}
				if queued := q.List(); len(queued) != 0 {
					t.Errorf("queued %v, want nothing queued", queued)
				}
			},
		},
		{
			name: "confirm Tool is refused with the Agent fallback",
			run: func(t *testing.T) {
				reg := tool.NewRegistry(time.Second)
				confirmCalls := &callCounter{}
				confirm := fakeTool("lights.confirm", func(context.Context, fakeIn) (fakeOut, error) {
					confirmCalls.inc()
					return fakeOut{Value: "confirmed"}, nil
				})
				confirm.Permission = tool.PermissionConfirm
				if err := tool.Add(reg, confirm); err != nil {
					t.Fatalf("Add lights.confirm: %v", err)
				}
				runner := newFakeRunner()
				runner.result = agent.Result{Text: "Agent answer."}
				// A live wording table may name a confirm Tool even though
				// pipeline.New rejects that shape in a code phrase table.
				p := livePhrasePipeline(t, reg, map[string]string{"turn off the lights": "lights.confirm"}, runner)
				q := &pipeline.DeferredQueue{}
				p.BindDeferred(q)

				reply := p.Handle(context.Background(), request(), "after the timer turn off the lights")
				if reply.Path != tool.PathAgent {
					t.Fatalf("path = %q, want %q (reply %q)", reply.Path, tool.PathAgent, reply.Text)
				}
				if got := q.List(); len(got) != 0 {
					t.Errorf("confirm action was queued: %+v", got)
				}
				if reply.Pending != nil {
					t.Error("confirm deferred action armed a Confirmation")
				}
				if got := confirmCalls.calls(); got != 0 {
					t.Errorf("confirm Tool handler ran %d times, want 0", got)
				}
				if got := len(runner.calls()); got != 1 {
					t.Fatalf("Agent calls = %d, want 1", got)
				}
			},
		},
		{
			name: "ninth queued action is refused with the full message",
			run: func(t *testing.T) {
				p, q, runner := deferredRoutingPipeline(t)
				utterance := "after the timer turn off the lights"
				var last pipeline.Reply
				for i := 0; i < 9; i++ {
					last = p.Handle(context.Background(), request(), utterance)
				}
				if !strings.Contains(last.Text, "The action queue is full.") {
					t.Fatalf("ninth reply = %q, want the queue-full message", last.Text)
				}
				if last.Path != tool.PathControlPhrase {
					t.Errorf("path = %q, want %q", last.Path, tool.PathControlPhrase)
				}
				if queued := q.List(); len(queued) != 8 {
					t.Fatalf("queued %d actions, want 8", len(queued))
				}
				if len(runner.calls()) != 0 {
					t.Fatalf("Agent saw %v, want no fallback for a full queue", runner.calls())
				}
			},
		},
		{
			name: "cancel queued actions empties the queue and a later Trigger dispatches nothing",
			run: func(t *testing.T) {
				p, q, runner := deferredRoutingPipeline(t)
				p.Handle(context.Background(), request(), "after the timer turn off the lights")
				if queued := q.List(); len(queued) != 1 {
					t.Fatalf("queued = %+v, want one action before cancel", queued)
				}
				reply := p.Handle(context.Background(), request(), "cancel queued actions")
				if reply.Path != tool.PathControlPhrase {
					t.Fatalf("path = %q, want %q (reply %q)", reply.Path, tool.PathControlPhrase, reply.Text)
				}
				if queued := q.List(); len(queued) != 0 {
					t.Fatalf("queued = %+v, want an empty queue after cancel", queued)
				}
				q.Trigger("timer")
				if _, ok := q.NextReady(); ok {
					t.Fatal("cancelled action dispatched after Trigger")
				}
				if len(runner.calls()) != 0 {
					t.Fatalf("Agent saw %v, want no fallback for a cancellation", runner.calls())
				}
			},
		},
		{
			name: "NextReady returns insertion order only after Trigger",
			run: func(t *testing.T) {
				q := &pipeline.DeferredQueue{}
				texts := []string{"first", "second", "third"}
				for _, text := range texts {
					if !q.Add("timer", text) {
						t.Fatalf("Add %q unexpectedly overflowed", text)
					}
				}
				if _, ok := q.NextReady(); ok {
					t.Fatal("NextReady returned an action before Trigger")
				}
				q.Trigger("timer")
				for i, want := range texts {
					got, ok := q.NextReady()
					if !ok || got.Text != want {
						t.Fatalf("NextReady %d = %+v/%v, want %q", i, got, ok, want)
					}
				}
				if _, ok := q.NextReady(); ok {
					t.Fatal("NextReady returned more than the queued actions")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}
