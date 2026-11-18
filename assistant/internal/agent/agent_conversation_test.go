package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestConversationTurnUsesTheLongerBudgetAndPrompt pins the cue-word seam: the
// same fake answer is cut on a default turn and spoken whole on a conversation
// turn, and only the conversation request carries the mode line.
func TestConversationTurnUsesTheLongerBudgetAndPrompt(t *testing.T) {
	long := textReply("One. Two. Three. Four. Five.")
	fake := newFakeProvider(t, long, long)
	server := fake.start()
	settings := testSettings(server.URL)
	settings.ConversationFunc = func(utterance string) string {
		if strings.Contains(utterance, "explain") {
			return "cue"
		}
		return ""
	}
	a := newTestAgent(t, settings, nil, nil)

	short := a.Run(context.Background(), tool.Request{ID: "req-short"}, "what time is it", nil)
	if !short.Truncated {
		t.Fatalf("default turn Truncated = false, want the short budget to cut it")
	}
	if got, want := short.Text, "One. Two."; got != want {
		t.Fatalf("default reply = %q, want %q", got, want)
	}
	if short.Conversation != "" {
		t.Errorf("default turn Conversation = %q, want empty", short.Conversation)
	}

	conversation := a.Run(context.Background(), tool.Request{ID: "req-cue"}, "explain gravity", nil)
	if conversation.Truncated {
		t.Fatalf("conversation turn Truncated = true, want the longer budget to keep the answer")
	}
	if got, want := conversation.Text, "One. Two. Three. Four. Five."; got != want {
		t.Fatalf("conversation reply = %q, want %q", got, want)
	}
	if conversation.Conversation != "cue" {
		t.Errorf("conversation kind = %q, want cue", conversation.Conversation)
	}

	defaultSys := systemText(t, fake.request(t, 0).Body)
	if strings.Contains(defaultSys, agent.ConversationNote) {
		t.Errorf("default system prompt contains the conversation note: %q", defaultSys)
	}
	cueSys := systemText(t, fake.request(t, 1).Body)
	if !strings.Contains(cueSys, agent.ConversationNote) {
		t.Errorf("conversation system prompt = %q, want the mode line", cueSys)
	}
	if got := fake.request(t, 0).Body["max_completion_tokens"]; got != float64(agent.DefaultMaxCompletionTokens) {
		t.Errorf("default max_completion_tokens = %v, want %d", got, agent.DefaultMaxCompletionTokens)
	}
	if got := fake.request(t, 1).Body["max_completion_tokens"]; got != float64(agent.DefaultConversationMaxCompletionTokens) {
		t.Errorf("conversation max_completion_tokens = %v, want %d", got, agent.DefaultConversationMaxCompletionTokens)
	}
}

// systemText returns the content of the request's one system message.
func systemText(t *testing.T, body map[string]any) string {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("request has no messages: %v", body)
	}
	system, _ := messages[0].(map[string]any)
	text, _ := system["content"].(string)
	return text
}
