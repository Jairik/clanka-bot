package pipeline_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// TestTimerLabelAndRemainingThroughPipelineAgainstMiniLM is the acceptance
// journey at the real routing seam: "set a pasta timer for 10 minutes" then
// "how long is left on the pasta timer" must reach timer.set and
// timer.remaining through real parse extraction and the real MiniLM service.
func TestTimerLabelAndRemainingThroughPipelineAgainstMiniLM(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("ROUTING_EVAL_EMBED_URL"))
	if endpoint == "" {
		t.Skip("set ROUTING_EVAL_EMBED_URL to the MiniLM embed service to run the labelled Timer journey")
	}
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	manager := timer.NewManager(func() time.Time { return fixed }, nil, nil)
	registry := tool.NewRegistry(5 * time.Second)
	if err := timer.Register(registry, manager); err != nil {
		t.Fatalf("register timer: %v", err)
	}
	cfg := loadRoutingConfig(t)
	router := pipeline.NewSemanticRouter(registry, embed.NewClient(endpoint, "sentence-transformers/all-MiniLM-L6-v2"), pipeline.SemanticConfig{
		Threshold: cfg.Semantic.Threshold,
		Margin:    cfg.Semantic.Margin,
		Joiners:   cfg.Semantic.Joiners,
		Timeout:   10 * time.Second,
	})
	ctx := context.Background()
	if err := router.Build(ctx); err != nil {
		t.Fatalf("build semantic router: %v", err)
	}
	logger, logBuf := testLogger()
	// The timer Tools are the only ones registered, so the table is empty; the
	// full Control phrase table needs clock and audio and is covered by the
	// routing evaluation.
	p, err := pipeline.New(registry, nil, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(ctx, request(), "set a pasta timer for 10 minutes")
	if reply.Tool != timer.SetName || reply.Path != tool.PathFast {
		t.Fatalf("set reply = %+v, want timer.set on the Fast path; log=%s", reply, logBuf.String())
	}
	list := manager.List()
	if len(list) != 1 || list[0].Label != "pasta" {
		t.Fatalf("timers after set = %+v, want one labelled pasta", list)
	}

	reply = p.Handle(ctx, request(), "how long is left on the pasta timer")
	if reply.Tool != timer.RemainingName || reply.Path != tool.PathFast {
		t.Fatalf("remaining reply = %+v, want timer.remaining on the Fast path; log=%s", reply, logBuf.String())
	}
	if !strings.Contains(reply.Text, "pasta") || !strings.Contains(reply.Text, "10 minutes") {
		t.Fatalf("remaining text = %q, want the label and 10 minutes", reply.Text)
	}

	// A list request stays on the Fast path and is never offered to the Agent
	// path; it is LocalOnlyYes.
	reply = p.Handle(ctx, request(), "what timers do I have")
	if reply.Tool != timer.ListName || reply.Path != tool.PathFast {
		t.Fatalf("list reply = %+v, want timer.list on the Fast path", reply)
	}
	info, ok := registry.Info(timer.ListName)
	if !ok || !info.LocalOnly {
		t.Fatalf("timer.list info = %+v, want LocalOnlyYes", info)
	}
	manager.Cancel("")
}
