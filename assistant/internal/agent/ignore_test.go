package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestIsIgnoreReplyMatchesTheTokenOnly proves the one rule the Agent and the
// reply session share: an ignore reply is the token, trimmed and case
// insensitive, never ordinary text that happens to mention ignoring.
func TestIsIgnoreReplyMatchesTheTokenOnly(t *testing.T) {
	tests := []struct {
		reply string
		want  bool
	}{
		{agent.IgnoreToken, true},
		{"  <ignore>\n", true},
		{"<IGNORE>", true},
		{"<ignore> extra text", true},
		{"", false},
		{"ignore", false},
		{"<ignore", false},
		{"It is sunny.", false},
		{"I will ignore that.", false},
	}
	for _, test := range tests {
		if got := agent.IsIgnoreReply(test.reply); got != test.want {
			t.Errorf("IsIgnoreReply(%q) = %v, want %v", test.reply, got, test.want)
		}
	}
}

// TestIgnoreReplyIsNotSpokenOrRemembered covers every streamed shape of the
// signal: the turn speaks nothing, leaves history empty, and the next request
// carries neither the ignored utterance nor the token (#498).
func TestIgnoreReplyIsNotSpokenOrRemembered(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{name: "alone", reply: textReply(agent.IgnoreToken)},
		{name: "surrounding whitespace", reply: textReply(" ", agent.IgnoreToken, "\n")},
		{name: "split across deltas", reply: textReply("<", "ign", "ore", ">")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeProvider(t, test.reply, textReply("It is sunny."))
			a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

			result := a.Run(context.Background(), tool.Request{ID: "req-ignore"}, "who left the fridge open", nil)
			if !result.Ignored {
				t.Fatalf("Ignored = false for %q", test.reply)
			}
			if result.Text != "" {
				t.Fatalf("Text = %q, want no spoken text for an ignored turn", result.Text)
			}
			if turns := a.History().Recent(); len(turns) != 0 {
				t.Fatalf("history = %+v, want the ignored turn left out", turns)
			}

			// The next real request must not see the ignored utterance or the
			// token: a misheard fragment cannot steer a later turn.
			second := a.Run(context.Background(), tool.Request{ID: "req-after"}, "what's the weather", nil)
			if second.Text != "It is sunny." {
				t.Fatalf("second reply = %q", second.Text)
			}
			raw, err := json.Marshal(fake.request(t, 1).Body["messages"])
			if err != nil {
				t.Fatalf("marshal second messages: %v", err)
			}
			var messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &messages); err != nil {
				t.Fatalf("unmarshal second messages: %v", err)
			}
			// The system part names the token by design; history and the
			// current utterance must carry neither the fragment nor the token.
			var conversation strings.Builder
			for _, message := range messages {
				if message.Role == "system" {
					continue
				}
				conversation.WriteString(message.Content)
			}
			lowered := strings.ToLower(conversation.String())
			for _, unwanted := range []string{"fridge", "ignore"} {
				if strings.Contains(lowered, unwanted) {
					t.Fatalf("second request carries %q: %s", unwanted, lowered)
				}
			}
			if !strings.Contains(lowered, "what's the weather") {
				t.Fatalf("second request is missing its own utterance: %s", lowered)
			}
		})
	}
}

// TestIgnoreAfterAToolRunIsNotErased keeps a turn that already acted: the
// signal only applies before any Tool ran, and a later token becomes an empty
// answer so it is never spoken and the history keeps what happened (#498).
func TestIgnoreAfterAToolRunIsNotErased(t *testing.T) {
	fake := newFakeProvider(t,
		toolCallReply("", fakeCall{ID: "call-1", Name: "time__now", Args: "{}"}),
		textReply(agent.IgnoreToken),
	)
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-tool-ignore"}, "what time is it", nil)
	if result.Ignored {
		t.Fatal("Ignored = true after a Tool ran; the turn must stay in history")
	}
	if strings.Contains(strings.ToLower(result.Text), "ignore") {
		t.Fatalf("Text = %q, want the token never spoken", result.Text)
	}
	turns := a.History().Recent()
	if len(turns) != 1 || turns[0].Reply != result.Text {
		t.Fatalf("history = %+v, want the executed turn recorded once", turns)
	}
}

// TestIgnoreReplyWithAStreamedDeltaStillLeavesNoHistory proves the onDelta
// callback cannot smuggle the turn in: the deltas are streamed before the final
// content is known, yet the recorded history stays empty.
func TestIgnoreReplyWithAStreamedDeltaStillLeavesNoHistory(t *testing.T) {
	fake := newFakeProvider(t, textReply("<", "ignore", ">"))
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

	var deltas []string
	result := a.Run(context.Background(), tool.Request{ID: "req-stream"}, "mumbling", func(delta string) {
		deltas = append(deltas, delta)
	})
	if !result.Ignored {
		t.Fatal("Ignored = false")
	}
	if strings.Join(deltas, "") != agent.IgnoreToken {
		t.Fatalf("deltas = %q, want the token streamed for the session to hold back", deltas)
	}
	if turns := a.History().Recent(); len(turns) != 0 {
		t.Fatalf("history = %+v, want no recorded turn", turns)
	}
}
