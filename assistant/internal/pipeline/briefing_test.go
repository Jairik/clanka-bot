package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/briefing"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestBriefingReachesComposer proves the morning-briefing stage intercepts a
// briefing phrase, makes no model call, records one briefing path in the request
// line, records one history turn with every part's call, and is Local-only.
func TestBriefingReachesComposer(t *testing.T) {
	reg, _, _, _ := evalRegistry(t)
	logger, buf := testLogger()
	composer := briefing.New(reg, time.Now, logger)
	history := agent.NewHistory(10, 0, nil)
	runner := newFakeRunner()

	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithBriefing(composer),
		pipeline.WithAgent(runner, nil),
		pipeline.WithHistory(history),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "good morning")

	if reply.Path != tool.PathBriefing {
		t.Fatalf("path = %q, want %q", reply.Path, tool.PathBriefing)
	}
	// The Agent path must not run for a briefing.
	if got := len(runner.utterances); got != 0 {
		t.Errorf("agent was called %d times, want 0", got)
	}
	// The four private sources make the whole reply Local-only.
	if !reply.LocalOnly {
		t.Error("briefing reply should be Local-only")
	}
	if reply.Text == "" {
		t.Fatal("briefing text is empty")
	}
	// The reliable clauses from the empty managers and store.
	for _, want := range []string{
		"No alarms are set.",
		"No timers are running.",
		"No reminders are set.",
		"Your to-do list is empty.",
	} {
		if !strings.Contains(reply.Text, want) {
			t.Errorf("text = %q, want it to contain %q", reply.Text, want)
		}
	}
	// One history turn carrying every part's call.
	turns := history.Recent()
	if len(turns) != 1 {
		t.Fatalf("history turns = %d, want 1", len(turns))
	}
	if got := len(turns[0].Calls); got != 5 {
		t.Errorf("history calls = %d, want 5 (every part)", got)
	}
	// The request log line records the briefing path.
	if !strings.Contains(buf.String(), `"path":"briefing"`) {
		t.Errorf("request line = %q, want path briefing", buf.String())
	}
}

// TestBriefingRouteOrder proves the briefing is checked before the chain
// planner (a compound briefing phrase is taken by the briefing, not chained)
// and after the Agent request prefix (an Agent-prefixed utterance still goes to
// the Agent).
func TestBriefingRouteOrder(t *testing.T) {
	reg, _, _, _ := evalRegistry(t)
	logger, _ := testLogger()
	composer := briefing.New(reg, time.Now, logger)

	// A briefing phrase containing a chain joiner is taken by the briefing.
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithBriefing(composer),
		pipeline.WithAgent(newFakeRunner(), []string{"ask"}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	p.BindChains(func() bool { return true })

	reply := p.Handle(context.Background(), request(), "what does my day look like and then set a timer for five minutes")
	if reply.Path != tool.PathBriefing {
		t.Fatalf("compound briefing path = %q, want %q", reply.Path, tool.PathBriefing)
	}
	if reply.Path == tool.PathFast {
		t.Error("the chain planner took the briefing phrase")
	}

	// An Agent-prefixed utterance still goes to the Agent, even when it names a
	// briefing phrase.
	runner := newFakeRunner()
	p2, err := pipeline.New(reg, nil, logger,
		pipeline.WithBriefing(composer),
		pipeline.WithAgent(runner, []string{"ask"}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	reply2 := p2.Handle(context.Background(), request(), "ask what's my day")
	if reply2.Path != tool.PathAgent {
		t.Fatalf("agent-prefixed path = %q, want %q", reply2.Path, tool.PathAgent)
	}
	if got := len(runner.utterances); got != 1 {
		t.Errorf("agent was called %d times, want 1", got)
	}
}
