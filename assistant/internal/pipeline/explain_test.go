package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

type explainIn struct {
	Query string `json:"query"`
}

type explainOut struct {
	Result string `json:"result"`
}

// TestExplainRoutingPathsAndNoExecution verifies that Explain returns the control-phrase
// path for an exact phrase, the Fast path with extracted arguments for a semantic match,
// and the Agent path when nothing matches, while keeping the tool handler counter at zero.
func TestExplainRoutingPathsAndNoExecution(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	var handlerCalls int

	// Register a tool with both examples and an extractor.
	extractTool := tool.Tool[explainIn, explainOut]{
		Name:        "test.search",
		Description: "Search tool with extraction.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Examples:    []string{"search for pizza"},
		Extract: func(text string) (explainIn, bool) {
			return explainIn{Query: text}, true
		},
		Handler: func(ctx context.Context, in explainIn) (explainOut, error) {
			handlerCalls++
			return explainOut{Result: "found " + in.Query}, nil
		},
		Speak: func(out explainOut) string { return out.Result },
	}
	if err := tool.Add(reg, extractTool); err != nil {
		t.Fatalf("Add test.search: %v", err)
	}

	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it":      {1, 0},
		"what's the time":      {1, 0},
		"tell me the time":     {1, 0},
		"search for pizza":     {0, 1},
		"find me some pizza":   {0, 0.95},
		"completely unrelated": {0.1, 0.1},
	}}

	logger, _ := testLogger()
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}

	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger,
		pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	ctx := context.Background()

	// Control-phrase path for exact phrase.
	exact := p.Explain(ctx, "what time is it")
	if exact.Path != tool.PathControlPhrase {
		t.Errorf("exact.Path = %q, want %q", exact.Path, tool.PathControlPhrase)
	}
	if exact.Tool != clock.Name {
		t.Errorf("exact.Tool = %q, want %q", exact.Tool, clock.Name)
	}

	// Fast path with extracted args for semantic match.
	semanticMatch := p.Explain(ctx, "find me some pizza")
	if semanticMatch.Path != tool.PathFast {
		t.Errorf("semanticMatch.Path = %q, want %q", semanticMatch.Path, tool.PathFast)
	}
	if semanticMatch.Tool != "test.search" {
		t.Errorf("semanticMatch.Tool = %q, want test.search", semanticMatch.Tool)
	}
	if len(semanticMatch.Args) == 0 {
		t.Errorf("semanticMatch.Args is empty, want extracted JSON args")
	} else {
		var in explainIn
		if err := json.Unmarshal(semanticMatch.Args, &in); err != nil || in.Query != "find me some pizza" {
			t.Errorf("unmarshal args: %v, got %+v", err, in)
		}
	}
	if len(semanticMatch.Top) == 0 {
		t.Errorf("semanticMatch.Top is empty, want scores")
	}

	// Agent path when nothing matches.
	unmatched := p.Explain(ctx, "completely unrelated")
	if unmatched.Path != tool.PathAgent {
		t.Errorf("unmatched.Path = %q, want %q", unmatched.Path, tool.PathAgent)
	}

	// Tool handler counter must remain zero.
	if handlerCalls != 0 {
		t.Errorf("handlerCalls = %d, want 0 (Explain must be side-effect free)", handlerCalls)
	}
}

// TestExplainDoesNotMutateState verifies that Explain does not settle pending confirmations,
// does not enqueue deferred actions, does not call focus holders, and does not run chain steps.
func TestExplainDoesNotMutateState(t *testing.T) {
	reg := tool.NewRegistry(time.Second)

	addCountingTool(t, reg, "lights.off", &callCounter{})
	timerCalls := &callCounter{}
	addCountingTool(t, reg, "timer.set", timerCalls)

	confirmTool := tool.Tool[explainIn, explainOut]{
		Name:        "mail.send",
		Description: "Send email requiring confirmation.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in explainIn) (explainOut, error) {
			return explainOut{Result: "sent"}, nil
		},
	}
	if err := tool.Add(reg, confirmTool); err != nil {
		t.Fatalf("Add mail.send: %v", err)
	}

	// Propose a tool call to create a pending confirmation.
	proposed, err := reg.Propose("mail.send", json.RawMessage(`{"query":"test"}`))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	holder := &focusHolder{name: "Spotify", intents: []focus.Intent{focus.Pause}}
	var fReg focus.Registry
	claim := fReg.Claim(focus.Music, holder)
	defer claim.Release()

	logger, _ := testLogger()
	p, err := pipeline.New(reg, map[string]string{
		"turn off the lights": "lights.off",
		"pause":               "pause",
		"set a timer":         "timer.set",
	}, logger, pipeline.WithFocus(&fReg),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	p.BindChains(func() bool { return true })
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)

	ctx := context.Background()

	// Explain on a confirmation phrase ("yes") should indicate confirmation without resolving it.
	confExplain := p.Explain(ctx, "yes")
	if confExplain.Path != tool.PathConfirmation {
		t.Errorf("confExplain.Path = %q, want %q", confExplain.Path, tool.PathConfirmation)
	}
	// Verify pending confirmation is NOT settled.
	if gotPending, ok := reg.Pending(); !ok || gotPending.Tool != proposed.Tool {
		t.Errorf("pending confirmation settled or altered: got %+v (ok=%v), want tool %s", gotPending, ok, proposed.Tool)
	}

	// Explain on a deferred utterance should not enqueue anything into DeferredQueue.
	defExplain := p.Explain(ctx, "after the timer, turn off the lights")
	if defExplain.Path != tool.PathControlPhrase {
		t.Errorf("defExplain.Path = %q, want %q", defExplain.Path, tool.PathControlPhrase)
	}
	if len(q.List()) != 0 {
		t.Errorf("DeferredQueue has %d items, want 0", len(q.List()))
	}

	// Explain on a focus phrase should not invoke the focus holder.
	focusExplain := p.Explain(ctx, "pause")
	if focusExplain.Path != tool.PathFocusIntent {
		t.Errorf("focusExplain.Path = %q, want %q", focusExplain.Path, tool.PathFocusIntent)
	}
	if len(holder.seen()) != 0 {
		t.Errorf("focus holder called %d times, want 0", len(holder.seen()))
	}

	// Explain on a chain utterance should not execute chain steps.
	chainExplain := p.Explain(ctx, "turn off the lights and set a timer")
	if !chainExplain.ChainSplit {
		t.Errorf("chainExplain.ChainSplit = false, want true")
	}
	if timerCalls.calls() != 0 {
		t.Errorf("chain step executed: timerCalls=%d", timerCalls.calls())
	}
}

// TestExplainMatchesHandleEventsDecisionAndReasons verifies that Explain returns
// the same path, tool, and fallthrough reasons as real HandleEvents requests.
func TestExplainMatchesHandleEventsDecisionAndReasons(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it":  {1, 0},
		"what's the time":  {1, 0},
		"tell me the time": {1, 0},
		"what is time":     {0.5, 0.5}, // below threshold
	}}
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}

	logger, _ := testLogger()
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "agent response"}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger,
		pipeline.WithSemanticRouter(router), pipeline.WithAgent(runner, nil))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	transcripts := []string{
		"what time is it",
		"what is time",
		"tell me something else entirely",
	}

	for _, text := range transcripts {
		t.Run(text, func(t *testing.T) {
			explainDecision := p.Explain(context.Background(), text)
			reply := p.HandleEvents(context.Background(), request(), text, pipeline.Events{})

			if explainDecision.Path != reply.Path {
				t.Errorf("Path mismatch: Explain got %q, HandleEvents got %q", explainDecision.Path, reply.Path)
			}
			if explainDecision.Tool != reply.Tool {
				t.Errorf("Tool mismatch: Explain got %q, HandleEvents got %q", explainDecision.Tool, reply.Tool)
			}
		})
	}
}
