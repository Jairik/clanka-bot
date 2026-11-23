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

// TestCutoverPromptOmitsLocalOnlyToolsAndResults is the ADR-0002 boundary a
// later Hermes hand-off must keep: Local-only Tools are not offered, a
// Local-only history turn is dropped whole, and a Local-only Tool result is
// replaced by a generic refusal even if a caller reaches ToolResult by
// mistake.
func TestCutoverPromptOmitsLocalOnlyToolsAndResults(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[struct{}, struct{ Secret string }]{
		Name:        "mail.inbox",
		Description: "Read the inbox.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (struct{ Secret string }, error) {
			return struct{ Secret string }{Secret: "doctor appointment"}, nil
		},
	}); err != nil {
		t.Fatalf("register local tool: %v", err)
	}
	if err := tool.Add(reg, tool.Tool[struct{}, struct{ Summary string }]{
		Name:        "weather.current",
		Description: "Current conditions.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (struct{ Summary string }, error) {
			return struct{ Summary string }{Summary: "sunny"}, nil
		},
	}); err != nil {
		t.Fatalf("register public tool: %v", err)
	}

	builder := agent.NewPromptBuilder("Be brief.")
	offered := builder.Tools(reg)
	rawTools, err := json.Marshal(offered)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	body := string(rawTools)
	if strings.Contains(body, "mail") || strings.Contains(body, "inbox") {
		t.Fatalf("Local-only Tool was offered: %s", body)
	}
	if !strings.Contains(body, "weather") {
		t.Fatalf("public Tool missing from the offer: %s", body)
	}

	messages := builder.Messages("and tomorrow?", []agent.Turn{{
		Utterance: "read my inbox",
		Reply:     "You have one email from the doctor.",
		Calls: []agent.ToolCall{{
			Name:      "mail.inbox",
			Result:    json.RawMessage(`{"secret":"doctor appointment"}`),
			LocalOnly: true,
		}},
	}})
	rawMessages, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	sent := string(rawMessages)
	for _, leaked := range []string{"read my inbox", "doctor", "email"} {
		if strings.Contains(sent, leaked) {
			t.Fatalf("Local-only history leaked %q: %s", leaked, sent)
		}
	}

	result := builder.ToolResult("call-1", json.RawMessage(`{"secret":"doctor appointment"}`), true)
	rawResult, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	if strings.Contains(string(rawResult), "doctor") {
		t.Fatalf("Local-only Tool result leaked: %s", rawResult)
	}
	var decoded struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rawResult, &decoded); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if decoded.Content != `{"error":"unavailable"}` {
		t.Fatalf("Local-only Tool result content = %q, want the generic refusal", decoded.Content)
	}
}

// TestCutoverEligibleHistoryIsThePublicTurnsOnly names the payload a later
// Hermes boundary may carry: public Fast-path turns and the current utterance,
// never a Local-only call. The shared History still keeps the private turn on
// the Pi.
func TestCutoverEligibleHistoryIsThePublicTurnsOnly(t *testing.T) {
	history := agent.NewHistory(10, 5*time.Minute, nil)
	history.Add(agent.Turn{
		Utterance: "read my inbox",
		Reply:     "You have one email.",
		Calls:     []agent.ToolCall{{Name: "mail.inbox", LocalOnly: true}},
	})
	history.Add(agent.Turn{
		Utterance: "what's the weather",
		Reply:     "It's sunny.",
		Calls:     []agent.ToolCall{{Name: "weather.current", Result: json.RawMessage(`{"summary":"sunny"}`)}},
	})

	recent := history.Recent()
	if len(recent) != 2 {
		t.Fatalf("Pi history = %+v, want both turns stored locally", recent)
	}

	sent, err := json.Marshal(agent.NewPromptBuilder("system").Messages("and tomorrow?", recent))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(sent)
	if strings.Contains(body, "inbox") || strings.Contains(body, "email") {
		t.Fatalf("eligible history included a Local-only turn: %s", body)
	}
	if !strings.Contains(body, "what's the weather") || !strings.Contains(body, "It's sunny.") {
		t.Fatalf("eligible history dropped the public turn: %s", body)
	}
	if !strings.Contains(body, "and tomorrow?") {
		t.Fatalf("current utterance missing: %s", body)
	}
}
