package pipeline_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func TestHandleEventsSignalsTheAgentPathBeforeTheFirstDelta(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "partial answer"}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	var order []string
	p.HandleEvents(context.Background(), request(), "tell me something", pipeline.Events{
		AgentPath: func() { order = append(order, "agent-path") },
		Delta:     func(string) { order = append(order, "delta") },
	})

	if want := []string{"agent-path", "delta"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("events = %v, want %v", order, want)
	}
}

func TestHandleEventsSkipsTheAgentPathSignalOnTheFastPath(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	runner := newFakeRunner()
	p := newAgentPipeline(t, runner, reg, map[string]string{"what time is it": clock.Name})

	signalled := false
	deltas := 0
	reply := p.HandleEvents(context.Background(), request(), "what time is it", pipeline.Events{
		AgentPath: func() { signalled = true },
		Delta:     func(string) { deltas++ },
	})

	if signalled {
		t.Fatal("Agent path was signalled on a Control phrase")
	}
	if deltas != 0 {
		t.Fatalf("deltas = %d, want none on the Fast path", deltas)
	}
	if reply.Path != tool.PathControlPhrase {
		t.Fatalf("path = %q", reply.Path)
	}
}
