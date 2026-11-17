package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// TestConversationTurnHookRunsOnEveryRequest pins the idle-clock seam: a
// Fast path request counts as a conversation turn too, not only an Agent turn.
func TestConversationTurnHookRunsOnEveryRequest(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	logger, _ := testLogger()
	touched := 0
	p, err := pipeline.New(reg, nil, logger, pipeline.WithConversationTurn(func() { touched++ }))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	p.Handle(context.Background(), request(), "what time is it")
	if touched != 1 {
		t.Fatalf("conversation turn hook ran %d times, want 1", touched)
	}
}
