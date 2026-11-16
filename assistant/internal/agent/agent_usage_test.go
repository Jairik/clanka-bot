package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// tokenCounts is a scripted final usage chunk for the fake provider.
type tokenCounts struct {
	prompt, completion, total, cached int64
}

// replyWithUsage renders a streamed reply whose final chunk reports token
// usage. A nil usage sends no usage chunk, like a provider that ignores the
// include_usage option and therefore reports nothing.
func replyWithUsage(content string, calls []fakeCall, usage *tokenCounts, finish string) string {
	var b strings.Builder
	deltas := make([]any, 0, len(calls))
	for i, call := range calls {
		deltas = append(deltas, map[string]any{
			"index": i,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Args,
			},
		})
	}
	delta := map[string]any{"role": "assistant"}
	if content != "" {
		delta["content"] = content
	}
	if len(deltas) > 0 {
		delta["tool_calls"] = deltas
	}
	writeChunk(&b, map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": nil,
		}},
	})
	if usage != nil {
		// The usage chunk carries an empty Choices array, which is why the
		// provider must read it before skipping choice-less chunks.
		writeChunk(&b, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     usage.prompt,
				"completion_tokens": usage.completion,
				"total_tokens":      usage.total,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": usage.cached,
				},
			},
		})
	}
	writeFinish(&b, finish)
	return b.String()
}

func TestTokenUsageIsParsedAndSummedAcrossSteps(t *testing.T) {
	fake := newFakeProvider(t,
		replyWithUsage("", []fakeCall{{ID: "call-1", Name: "time__now", Args: "{}"}},
			&tokenCounts{prompt: 100, completion: 10, total: 110, cached: 20}, "tool_calls"),
		replyWithUsage("It's 3:04 PM.", nil,
			&tokenCounts{prompt: 50, completion: 5, total: 55, cached: 10}, "stop"),
	)
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-usage"}, "what time is it", nil)
	if result.Text != "It's 3:04 PM." {
		t.Fatalf("reply = %q", result.Text)
	}
	if result.PromptTokens != 150 || result.CompletionTokens != 15 || result.TotalTokens != 165 || result.CachedTokens != 30 {
		t.Fatalf("usage = %+v, want 150/15/165/30 summed over two steps", result)
	}
	if result.Steps != 2 {
		t.Fatalf("steps = %d, want 2", result.Steps)
	}

	// The provider must ask for the usage chunk on every model call.
	for i := 0; i < fake.requestCount(); i++ {
		opts, ok := fake.request(t, i).Body["stream_options"].(map[string]any)
		if !ok || opts["include_usage"] != true {
			t.Errorf("request %d stream_options = %v, want include_usage true", i, fake.request(t, i).Body["stream_options"])
		}
	}
}

func TestTokenUsageIsZeroWhenProviderOmitsIt(t *testing.T) {
	// textReply has no usage chunk, like a provider that ignores include_usage.
	fake := newFakeProvider(t, textReply("Hello."))
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-no-usage"}, "hello", nil)
	if result.PromptTokens != 0 || result.CompletionTokens != 0 || result.TotalTokens != 0 || result.CachedTokens != 0 {
		t.Fatalf("usage = %+v, want all zero when the provider omits usage", result)
	}
	if result.Steps != 1 {
		t.Fatalf("steps = %d, want 1", result.Steps)
	}
}
