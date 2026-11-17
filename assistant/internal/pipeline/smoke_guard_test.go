package pipeline_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Smoke probes must refuse a misrouted action before invoking its handler.
func TestSmokeRejectsRemappedAction(t *testing.T) {
	calls := 0
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[struct{}, string]{
		Name: "test.act", Description: "Changes state",
		Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			calls++
			return "changed", nil
		},
		Speak: func(s string) string { return s },
	}); err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(reg, map[string]string{"what time is it": "test.act"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := latency.WithContext(context.Background(), latency.New("review-smoke", latency.SourceSmoke))
	p.Handle(ctx, tool.Request{ID: "review-smoke"}, "what time is it")
	if calls != 0 {
		t.Fatalf("smoke executed action handler %d times", calls)
	}
}

// A smoke turn must not dispatch a live phrase remapped to a Focus intent.
func TestSmokeRejectsFocusRemap(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	holder := &focusHolder{name: "Spotify", intents: []focus.Intent{focus.Pause}}
	fReg := focus.New()
	claim := fReg.Claim(focus.Music, holder)
	defer claim.Release()
	logger, _ := testLogger()
	p, err := pipeline.New(reg, map[string]string{"what time is it": "pause"}, logger, pipeline.WithFocus(fReg))
	if err != nil {
		t.Fatal(err)
	}
	ctx := latency.WithContext(t.Context(), latency.New("smoke-focus", latency.SourceSmoke))
	p.Handle(ctx, tool.Request{ID: "smoke-focus"}, "what time is it")
	if len(holder.seen()) != 0 {
		t.Fatalf("smoke dispatched Focus: %v", holder.seen())
	}
}
