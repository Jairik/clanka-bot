package pipeline_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if len(q.Take("music")) != 0 || len(q.Take("timer")) != 7 || len(q.Take("timer")) != 0 {
		t.Fatal("incorrect consumption")
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
