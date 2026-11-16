package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func agentUsageLine(t *testing.T, timeline *latency.Timeline) (map[string]any, string) {
	t.Helper()
	var rendered bytes.Buffer
	timeline.Complete(slog.New(slog.NewJSONHandler(&rendered, nil)), latency.OutcomeNoAudio, nil)
	var line map[string]any
	if err := json.Unmarshal(rendered.Bytes(), &line); err != nil {
		t.Fatalf("request line is not JSON: %v\n%s", err, rendered.String())
	}
	return line, rendered.String()
}

// TestAgentRequestLineCarriesTokenCountsWithoutText proves the counts reach the
// one request line the way agent_retries does, and that neither the model
// prompt nor the reply text is ever part of those attributes.
func TestAgentRequestLineCarriesTokenCountsWithoutText(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{
		Text:             "REPLY_SECRET_MARKER must stay off the request line",
		PromptTokens:     12000,
		CompletionTokens: 34,
		TotalTokens:      12034,
		CachedTokens:     512,
		Steps:            2,
	}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	timeline := latency.New("req-usage", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)
	p.Handle(ctx, request(), "ask the agent about the weather")

	line, raw := agentUsageLine(t, timeline)
	for key, want := range map[string]float64{
		"prompt_tokens":     12000,
		"completion_tokens": 34,
		"total_tokens":      12034,
		"cached_tokens":     512,
		"agent_steps":       2,
	} {
		got, ok := line[key].(float64)
		if !ok {
			t.Errorf("%s = %v (%T), want a number", key, line[key], line[key])
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if strings.Contains(raw, "REPLY_SECRET_MARKER") {
		t.Error("reply text leaked into the request line")
	}
}

// TestNonAgentRequestLineHasNoTokenCounts proves only the Agent path publishes
// usage attributes; a Fast-path request carries neither key.
func TestNonAgentRequestLineHasNoTokenCounts(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	p := newAgentPipeline(t, newFakeRunner(), reg, map[string]string{"what time is it": clock.Name})

	timeline := latency.New("req-fast", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)
	p.Handle(ctx, request(), "what time is it")

	line, _ := agentUsageLine(t, timeline)
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "agent_steps"} {
		if value, ok := line[key]; ok {
			t.Errorf("%s = %v present on a non-Agent request line", key, value)
		}
	}
}
