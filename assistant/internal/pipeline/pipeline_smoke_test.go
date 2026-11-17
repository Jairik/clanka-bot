package pipeline_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// TestPipelineSourceSmokeDoesNotOverwriteLastReply asserts that turns with
// SourceSmoke do not write to the lastReply store.
func TestPipelineSourceSmokeDoesNotOverwriteLastReply(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}

	lastStore := lastreply.New()
	lastStore.Record(lastreply.Reply{
		Text: "original user reply",
		Tool: "some.tool",
		Path: string(tool.PathFast),
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pipeline.New(reg, map[string]string{
		"what time is it": clock.Name,
	}, logger, pipeline.WithLastReply(lastStore))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	tl := latency.New("smoke-req-1", latency.SourceSmoke)
	ctx := latency.WithContext(context.Background(), tl)

	reply := p.Handle(ctx, tool.Request{ID: "smoke-req-1"}, "what time is it")
	if reply.Text == "" {
		t.Fatal("expected reply from pipeline")
	}

	got, ok := lastStore.Last()
	if !ok {
		t.Fatal("expected lastReply to exist")
	}
	if got.Text != "original user reply" {
		t.Fatalf("smoke turn overwrote lastReply: got %q, want %q", got.Text, "original user reply")
	}
}

// TestPipelineSourceSmokePreservesPendingConfirmation proves that an existing
// pending confirmation is neither resolved nor dropped by a smoke turn.
func TestPipelineSourceSmokePreservesPendingConfirmation(t *testing.T) {
	reg := tool.NewRegistry(time.Minute)
	fixed := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	if err := tool.Add(reg, tool.Tool[struct{}, string]{
		Name:        "test.confirm",
		Description: "Confirm an action.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Handler:     func(context.Context, struct{}) (string, error) { return "done", nil },
		Speak:       func(out string) string { return out },
	}); err != nil {
		t.Fatalf("add confirm tool: %v", err)
	}

	// Arm a pending confirmation in the registry.
	pending, err := reg.Propose("test.confirm", nil)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := pipeline.New(reg, map[string]string{
		"what time is it": clock.Name,
	}, logger, pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	tl := latency.New("smoke-req-2", latency.SourceSmoke)
	ctx := latency.WithContext(context.Background(), tl)

	// Even though "what time is it" is not a "yes", it should NOT drop the pending call.
	p.Handle(ctx, tool.Request{ID: "smoke-req-2"}, "what time is it")

	currentPending, ok := reg.Pending()
	if !ok {
		t.Fatal("smoke turn dropped pending confirmation")
	}
	if currentPending.Tool != pending.Tool {
		t.Fatalf("pending confirmation Tool changed: got %q, want %q", currentPending.Tool, pending.Tool)
	}
}

// TestPipelineSourceSmokeSkipsAgentModelCall proves that when the smoke turn
// falls through to the Agent path, it marks PathAgent without invoking the model runner.
func TestPipelineSourceSmokeSkipsAgentModelCall(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	runner := newFakeRunner()
	runner.result = agent.Result{Text: "expensive model answer"}

	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(runner, nil),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	tl := latency.New("smoke-req-3", latency.SourceSmoke)
	ctx := latency.WithContext(context.Background(), tl)

	reply := p.Handle(ctx, tool.Request{ID: "smoke-req-3"}, "what time is it")
	if reply.Path != tool.PathAgent {
		t.Fatalf("expected PathAgent, got %v", reply.Path)
	}
	if reply.Text == "expensive model answer" {
		t.Fatal("smoke turn invoked the agent runner")
	}

	runner.mu.Lock()
	invoked := len(runner.utterances)
	runner.mu.Unlock()
	if invoked != 0 {
		t.Fatalf("runner was called %d times; expected 0", invoked)
	}
}
