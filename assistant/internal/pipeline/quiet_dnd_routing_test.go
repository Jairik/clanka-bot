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
	quiettools "github.com/Jairik/clanka-bot/assistant/tools/quiet"
)

// TestQuietForDurationsReachDNDThroughPipeline is the #363 acceptance journey
// at the real routing seam: the plain "quiet for <duration>" family must reach
// quiet.dnd on the semantic Fast path (with quiet.off as the competing Tool)
// and speak the right period, while clock-time windows stay with quiet.set.
// It needs the real MiniLM service because the gap is a margin between two
// embedded Example sets, which no deterministic stand-in can reproduce.
func TestQuietForDurationsReachDNDThroughPipeline(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("ROUTING_EVAL_EMBED_URL"))
	if endpoint == "" {
		t.Skip("set ROUTING_EVAL_EMBED_URL to the MiniLM embed service to run the quiet.dnd routing journey")
	}

	cfg := loadRoutingConfig(t)
	reg, _, _, _ := evalRegistry(t)
	router := pipeline.NewSemanticRouter(reg, embed.NewClient(endpoint, "sentence-transformers/all-MiniLM-L6-v2"), pipeline.SemanticConfig{
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
	p, err := pipeline.New(reg, cfg.ControlPhrases, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	for _, tc := range []struct {
		utterance string
		wantText  string
	}{
		{"quiet for 0.5 hours", "Do not disturb for 30 minutes."},
		{"quiet for half an hour", "Do not disturb for 30 minutes."},
		{"quiet for 15 minutes", "Do not disturb for 15 minutes."},
		{"quiet for 2 hours", "Do not disturb for 2 hours."},
	} {
		reply := p.Handle(ctx, request(), tc.utterance)
		if reply.Tool != quiettools.DNDName || reply.Path != tool.PathFast {
			t.Errorf("%q: reply = %+v, want %s on the Fast path; log=%s", tc.utterance, reply, quiettools.DNDName, logBuf.String())
			continue
		}
		if reply.Text != tc.wantText {
			t.Errorf("%q: text = %q, want %q", tc.utterance, reply.Text, tc.wantText)
		}
	}

	// A real daily window keeps its own Tool; the decimal form above must not
	// steal these clock-time phrases.
	for _, utterance := range []string{
		"set quiet hours from 10:30 to 7",
		"quiet from 22:00 to 07:00",
	} {
		reply := p.Handle(ctx, request(), utterance)
		if reply.Tool != quiettools.SetName || reply.Path != tool.PathFast {
			t.Errorf("%q: reply = %+v, want %s on the Fast path; log=%s", utterance, reply, quiettools.SetName, logBuf.String())
		}
	}
}
