package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestIgnoredTurnCarriesNoTextAndKeepsTheLastReply proves the pipeline's side
// of the ignore signal: the reply has no words to speak and the turn the user
// last actually heard stays the Last reply (#498).
func TestIgnoredTurnCarriesNoTextAndKeepsTheLastReply(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "It is sunny."}
	store := lastreply.New()
	logger, _ := testLogger()
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithLastReply(store),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	if reply := p.Handle(context.Background(), request(), "will it rain"); reply.Text != "It is sunny." {
		t.Fatalf("first reply = %q", reply.Text)
	}
	runner.result = agent.Result{Ignored: true}
	ignored := p.Handle(context.Background(), request(), "who left the fridge open")
	if !ignored.Ignored {
		t.Fatal("Ignored = false, want the Agent's signal carried through")
	}
	if ignored.Text != "" {
		t.Fatalf("Text = %q, want no spoken words for an ignored turn", ignored.Text)
	}
	last, ok := store.Last()
	if !ok || last.Text != "It is sunny." {
		t.Fatalf("Last reply = %+v, %v, want the earlier answer untouched", last, ok)
	}
}
