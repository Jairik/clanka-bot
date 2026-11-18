package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestStreamedReplyIsCleanedAndCutAtTheSpokenBudget(t *testing.T) {
	fake := newFakeProvider(t, textReply(
		"**Sure.** Here is the list.\n",
		"- Milk.\n",
		"- Eggs 😊.\n",
		"Third sentence. Fourth sentence.",
	))
	server := fake.start()
	a := newTestAgent(t, testSettings(server.URL), nil, nil)

	var spoken strings.Builder
	result := a.Run(context.Background(), tool.Request{ID: "req-budget"}, "what should I buy", func(delta string) {
		spoken.WriteString(delta)
	})

	if !result.Truncated {
		t.Fatalf("result.Truncated = false, want the budget to cut the reply")
	}
	want := "Sure. Here is the list."
	if result.Text != want {
		t.Fatalf("reply = %q, want %q", result.Text, want)
	}
	if got := spoken.String(); got != want {
		t.Errorf("streamed text = %q, want %q", got, want)
	}
	if strings.ContainsAny(result.Text, "**😊") || strings.Contains(result.Text, "- ") {
		t.Errorf("markdown or emoji reached the spoken text: %q", result.Text)
	}
	history := a.History().Recent()
	if len(history) != 1 || history[0].Reply != want {
		t.Errorf("history = %+v, want the cleaned truncated reply", history)
	}
}

func TestStreamingRequestCarriesTheCompletionTokenCap(t *testing.T) {
	fake := newFakeProvider(t, textReply("Hello."))
	server := fake.start()
	a := newTestAgent(t, testSettings(server.URL), nil, nil)
	a.Run(context.Background(), tool.Request{ID: "req-cap"}, "hello", nil)

	if got := fake.request(t, 0).Body["max_completion_tokens"]; got != float64(agent.DefaultMaxCompletionTokens) {
		t.Fatalf("max_completion_tokens = %v (%T), want %d", got, got, agent.DefaultMaxCompletionTokens)
	}
}

// truncatedToolCallReply streams a Tool call whose arguments are still
// arriving, then enough text to reach the spoken budget. The client cancels
// the stream mid-call, so the call must never reach the loop.
func truncatedToolCallReply(content string, call fakeCall) string {
	var b strings.Builder
	b.WriteString(toolCallChunk(call))
	b.WriteString(streamChunk(content))
	writeFinish(&b, "tool_calls")
	return b.String()
}

func TestSpokenBudgetCutEndsTheTurnWithoutRunningToolCalls(t *testing.T) {
	fake := newFakeProvider(t,
		truncatedToolCallReply(
			"Sure. Here is the answer. Third sentence.",
			fakeCall{ID: "call-1", Name: "time.now", Args: `{"city":`},
		),
		textReply("This second step must not run."),
	)
	server := fake.start()
	a := newTestAgent(t, testSettings(server.URL), nil, nil)

	var spoken strings.Builder
	result := a.Run(context.Background(), tool.Request{ID: "req-truncated-tool"}, "what time is it", func(delta string) {
		spoken.WriteString(delta)
	})

	if !result.Truncated {
		t.Fatalf("result.Truncated = false, want the budget to cut the reply")
	}
	want := "Sure. Here is the answer."
	if result.Text != want {
		t.Fatalf("reply = %q, want %q", result.Text, want)
	}
	if got := spoken.String(); got != want {
		t.Errorf("streamed text = %q, want %q", got, want)
	}
	if len(result.Calls) != 0 {
		t.Errorf("result.Calls = %+v, want the half-written call dropped", result.Calls)
	}
	if result.Steps != 1 {
		t.Errorf("steps = %d, want the turn to end after the truncated step", result.Steps)
	}
	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want no further model step", got)
	}
}

func TestLiveLimitsOverrideTheCompletionTokenCapPerTurn(t *testing.T) {
	fake := newFakeProvider(t, textReply("Hello."))
	server := fake.start()
	settings := testSettings(server.URL)
	// A live source overrides only the fields it sets; the rest keep the code
	// defaults, which is the seam conversation mode uses for one turn.
	settings.LimitsFunc = func() agent.Limits { return agent.Limits{MaxCompletionTokens: 150} }
	a := newTestAgent(t, settings, nil, nil)
	a.Run(context.Background(), tool.Request{ID: "req-live-cap"}, "hello", nil)

	if got := fake.request(t, 0).Body["max_completion_tokens"]; got != float64(150) {
		t.Fatalf("max_completion_tokens = %v (%T), want the live 150", got, got)
	}
}
