package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestAgentTruncationIsRecordedOnTheRequestLine proves the spoken-budget cut
// is visible on the one request line without looking like a failure.
func TestAgentTruncationIsRecordedOnTheRequestLine(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Short answer.", Truncated: true}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	timeline := latency.New("req-truncated", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)
	p.Handle(ctx, request(), "ask the agent for the long version")

	line, _ := agentUsageLine(t, timeline)
	if got := line["agent_truncated"]; got != true {
		t.Errorf("agent_truncated = %v (%T), want true", got, got)
	}
}
