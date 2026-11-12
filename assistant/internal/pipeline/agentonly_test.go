package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// agentOnlyTool is the regression shape: AgentOnly with a Speak renderer but
// no Examples. A Control phrase must still be refused, because a Control
// phrase is a Fast-path route.
func agentOnlyTool(name string) tool.Tool[fakeIn, fakeOut] {
	return tool.Tool[fakeIn, fakeOut]{
		Name:        name,
		Description: "Agent-only test tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   true,
		Handler: func(context.Context, fakeIn) (fakeOut, error) {
			return fakeOut{Value: name}, nil
		},
		Speak: func(out fakeOut) string { return out.Value },
	}
}

func TestControlPhraseCannotTargetAnAgentOnlyTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, agentOnlyTool("web.search")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	logger, _ := testLogger()
	_, err := pipeline.New(reg, map[string]string{"search the web": "web.search"}, logger)
	if err == nil {
		t.Fatal("pipeline.New accepted an AgentOnly Control-phrase target")
	}
	if !strings.Contains(err.Error(), "AgentOnly") {
		t.Fatalf("error = %v, want it to name the AgentOnly rule", err)
	}
}

func TestAgentOnlyToolStaysOffEveryFastPathHook(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, agentOnlyTool("web.search")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	info, ok := reg.Info("web.search")
	if !ok {
		t.Fatal("AgentOnly Tool disappeared from the registry")
	}
	if pipeline.FastPathEligible(info) {
		t.Fatal("AgentOnly Tool is Fast-path eligible")
	}
	if pipeline.CanUseFastPath(info) {
		t.Fatal("AgentOnly Tool can use the Fast path")
	}

	// Registration rejects the only shape that could have been indexed.
	tricky := agentOnlyTool("web.fetch")
	tricky.Examples = []string{"fetch example dot com"}
	if err := tool.Add(reg, tricky); err == nil {
		t.Fatal("registry accepted AgentOnly with Examples")
	}

	// A built set therefore cannot contain it, even when another Tool routes.
	normal := fakeTool("time.now", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "3pm"}, nil
	})
	normal.Examples = []string{"what time is it"}
	if err := tool.Add(reg, normal); err != nil {
		t.Fatalf("Add normal: %v", err)
	}
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it": {1, 0},
	}}
	router := pipeline.NewSemanticRouter(reg, embedder, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := router.ToolCount(); got != 1 {
		t.Fatalf("router indexed %d Tools, want 1 (the normal Tool)", got)
	}
	if got := router.Match(context.Background(), "what time is it"); got.Tool != "time.now" {
		t.Fatalf("semantic match = %q, want time.now", got.Tool)
	}
}
