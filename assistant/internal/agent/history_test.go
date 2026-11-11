package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

func TestHistoryKeepsLastTurnsAndClearsAfterQuiet(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	history := agent.NewHistory(2, 5*time.Minute, func() time.Time { return now })

	history.Add(agent.Turn{Utterance: "one", Reply: "1"})
	history.Add(agent.Turn{Utterance: "two", Reply: "2"})
	history.Add(agent.Turn{Utterance: "three", Reply: "3"})

	turns := history.Recent()
	if len(turns) != 2 || turns[0].Utterance != "two" || turns[1].Utterance != "three" {
		t.Fatalf("turns = %+v, want the last two", turns)
	}

	// Inside the quiet window the conversation survives.
	now = now.Add(4 * time.Minute)
	if len(history.Recent()) != 2 {
		t.Fatal("history cleared inside the quiet window")
	}

	// Past the window it is gone.
	now = now.Add(6 * time.Minute)
	if turns := history.Recent(); len(turns) != 0 {
		t.Fatalf("turns = %+v, want history cleared after the quiet window", turns)
	}
}

func TestPromptDropsLocalOnlyTurns(t *testing.T) {
	builder := agent.NewPromptBuilder("system")
	messages := builder.Messages("and tomorrow?", []agent.Turn{
		{Utterance: "read my inbox", Reply: "You have one email.", Calls: []agent.ToolCall{{
			Name:      "mail.inbox",
			LocalOnly: true,
		}}},
		{Utterance: "what time is it", Reply: "It's 3:04 PM."},
	})

	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "read my inbox") || strings.Contains(body, "one email") {
		t.Fatalf("Local-only turn leaked into the request: %s", body)
	}
	if !strings.Contains(body, "what time is it") {
		t.Fatalf("normal history missing from the request: %s", body)
	}
	if !strings.Contains(body, "and tomorrow?") {
		t.Fatalf("current utterance missing from the request: %s", body)
	}
}

func TestHistoryTurnsCarryCompactToolJSON(t *testing.T) {
	builder := agent.NewPromptBuilder("system")
	messages := builder.Messages("and tomorrow?", []agent.Turn{{
		Utterance: "what's the weather today",
		Reply:     "It's sunny.",
		Calls: []agent.ToolCall{{
			Name:   "weather.current",
			Args:   json.RawMessage(`{"day":"today"}`),
			Result: json.RawMessage(`{"summary":"sunny"}`),
		}},
	}})

	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	if !strings.Contains(string(raw), `weather.current`) || !strings.Contains(string(raw), `sunny`) {
		t.Fatalf("history is missing the compact Tool call: %s", raw)
	}
}

func TestMatchRequestPrefix(t *testing.T) {
	prefixes := []string{"ask the agent", "agent", "hey agent"}
	cases := []struct {
		text    string
		rest    string
		matched bool
	}{
		{"ask the agent what's the weather", "what's the weather", true},
		{"Agent, what time is it?", "what time is it", true},
		{"hey agent what's on my calendar", "what's on my calendar", true},
		{"agent", "", true},
		{"Hey agent!", "", true},
		{"what time is it", "", false},
		{"agentic workflows please", "", false},
		{"the agent should handle this", "", false},
	}
	for _, tc := range cases {
		rest, matched := agent.MatchRequestPrefix(tc.text, prefixes)
		if matched != tc.matched || rest != tc.rest {
			t.Errorf("MatchRequestPrefix(%q) = %q, %v; want %q, %v", tc.text, rest, matched, tc.rest, tc.matched)
		}
	}
}

func TestFollowUpSendsTheSharedHistory(t *testing.T) {
	fake := newFakeProvider(t, textReply("It will rain."))
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)
	a.History().Add(agent.Turn{
		Utterance: "what's the weather today",
		Reply:     "It's sunny.",
		Calls: []agent.ToolCall{{
			Name:   "weather.current",
			Args:   json.RawMessage(`{"day":"today"}`),
			Result: json.RawMessage(`{"summary":"sunny"}`),
		}},
	})

	if result := a.Run(context.Background(), tool.Request{ID: "req-9"}, "what about tomorrow?", nil); result.Text != "It will rain." {
		t.Fatalf("reply = %q", result.Text)
	}
	body := bodyString(t, fake.request(t, 0).Body)
	for _, want := range []string{"what's the weather today", "It's sunny.", "what about tomorrow?"} {
		if !strings.Contains(body, want) {
			t.Errorf("request is missing %q: %s", want, body)
		}
	}
}
