package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestExplicitHermesRouteStaysOutOfAgentHistory(t *testing.T) {
	runner := newFakeRunner()
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, nil,
		pipeline.WithAgent(runner, []string{"ask"}),
		pipeline.WithHermes(func(_ context.Context, task string) string {
			if task != "Explain HTTP" {
				t.Fatalf("got task %q", task)
			}
			return "Hermes is working on it."
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	reply := p.Handle(t.Context(), request(), "Ask Hermes: Explain HTTP")
	if reply.Path != tool.PathHermes || !reply.LocalOnly || reply.Text != "Hermes is working on it." {
		t.Fatalf("unexpected reply %+v", reply)
	}
	if len(runner.calls()) != 0 {
		t.Fatal("explicit Hermes input reached the Agent path")
	}
}
