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

// systemMessageOf pulls the one system message from a recorded provider body.
func systemMessageOf(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("request has no messages: %#v", body["messages"])
	}
	first, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("first message is %T, want an object", raw[0])
	}
	if first["role"] != "system" {
		t.Fatalf("first message role = %v, want system", first["role"])
	}
	content, _ := first["content"].(string)
	return content
}

// TestPersonaTravelsOnlyInTheLabelledSystemPart proves ADR-0002's label rule:
// the personality prompt is in the one system message, and the user part is
// unchanged. No extra request or message is added.
func TestPersonaTravelsOnlyInTheLabelledSystemPart(t *testing.T) {
	fake := newFakeProvider(t, textReply("GLaDOS online."))
	server := fake.start()
	settings := testSettings(server.URL)
	settings.PersonaFunc = func() agent.Persona {
		return agent.Persona{Name: "GLaDOS", Prompt: "Speak with dry precision and no filler."}
	}

	a := newTestAgent(t, settings, nil, nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-persona"}, "hello", nil)
	if result.Text != "GLaDOS online." {
		t.Fatalf("reply = %q", result.Text)
	}
	if count := fake.requestCount(); count != 1 {
		t.Fatalf("provider requests = %d, want exactly one", count)
	}
	body := fake.request(t, 0).Body
	system := systemMessageOf(t, body)
	if !strings.Contains(system, `Persona instructions for "GLaDOS":`) {
		t.Fatalf("system message has no persona label:\n%s", system)
	}
	if !strings.Contains(system, "Speak with dry precision and no filler.") {
		t.Fatalf("system message has no persona prompt:\n%s", system)
	}
	// The user message must not gain a second copy or an unlabelled persona.
	messages := body["messages"].([]any)
	user, _ := messages[len(messages)-1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("last message role = %v, want user", user["role"])
	}
	if text, _ := user["content"].(string); text != "hello" {
		t.Fatalf("user message = %q, want the unchanged utterance", text)
	}
}

// TestPersonaPromptIsSnapshottedOnceForTheWholeTurn proves a multi-step turn
// keeps the bundle it started with even though the source would return a new
// one on the next read.
func TestPersonaPromptIsSnapshottedOnceForTheWholeTurn(t *testing.T) {
	fake := newFakeProvider(t,
		toolCallReply("", fakeCall{ID: "call-1", Name: "test__echo", Args: "{}"}),
		textReply("Done."),
	)
	server := fake.start()
	settings := testSettings(server.URL)
	reads := 0
	settings.PersonaFunc = func() agent.Persona {
		reads++
		if reads == 1 {
			return agent.Persona{Name: "GLaDOS", Prompt: "First persona prompt."}
		}
		return agent.Persona{Name: "Alfred", Prompt: "Second persona prompt."}
	}
	registry := tool.NewRegistry(time.Second)
	addTool(t, registry, tool.Tool[struct{}, string]{
		Name:        "test.echo",
		Description: "echo",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler:     func(context.Context, struct{}) (string, error) { return "ok", nil },
		Examples:    []string{"echo something"},
	})

	a := newTestAgent(t, settings, registry, nil)
	// Building the Agent path must not read the Persona or call the provider:
	// nobody has asked anything yet.
	if reads != 0 {
		t.Fatalf("persona source read %d times during construction, want 0", reads)
	}
	if count := fake.requestCount(); count != 0 {
		t.Fatalf("provider called %d times during construction, want 0", count)
	}
	result := a.Run(context.Background(), tool.Request{ID: "req-multi"}, "use the tool", nil)
	if result.Text != "Done." {
		t.Fatalf("reply = %q", result.Text)
	}
	if reads != 1 {
		t.Fatalf("persona source read %d times in one turn, want 1", reads)
	}
	if count := fake.requestCount(); count != 2 {
		t.Fatalf("provider requests = %d, want 2 for the two model steps", count)
	}
	for i := 0; i < 2; i++ {
		system := systemMessageOf(t, fake.request(t, i).Body)
		if !strings.Contains(system, "First persona prompt.") || strings.Contains(system, "Second persona prompt.") {
			t.Fatalf("model step %d saw a different persona bundle:\n%s", i, system)
		}
	}
}

// TestPersonaChangesApplyOnTheNextTurn shows the snapshot boundary: a switch
// between turns is picked up, but the previous request is never rewritten.
func TestPersonaChangesApplyOnTheNextTurn(t *testing.T) {
	fake := newFakeProvider(t, textReply("First."), textReply("Second."))
	server := fake.start()
	settings := testSettings(server.URL)
	prompt := "First persona prompt."
	settings.PersonaFunc = func() agent.Persona {
		return agent.Persona{Name: "GLaDOS", Prompt: prompt}
	}
	a := newTestAgent(t, settings, nil, nil)
	a.Run(context.Background(), tool.Request{ID: "req-1"}, "one", nil)
	prompt = "Second persona prompt."
	a.Run(context.Background(), tool.Request{ID: "req-2"}, "two", nil)

	if system := systemMessageOf(t, fake.request(t, 0).Body); !strings.Contains(system, "First persona prompt.") {
		t.Fatalf("first turn lost its persona:\n%s", system)
	}
	if system := systemMessageOf(t, fake.request(t, 1).Body); !strings.Contains(system, "Second persona prompt.") {
		t.Fatalf("second turn did not pick up the new persona:\n%s", system)
	}
}

// TestNoPersonaLeavesTheSystemPartUnchanged keeps the no-Persona path free of
// new labels or empty sections.
func TestNoPersonaLeavesTheSystemPartUnchanged(t *testing.T) {
	builder := agent.NewPromptBuilder("Base prompt.")
	system := systemMessageOfPrompt(t, builder.Messages("hello", nil))
	if system != "Base prompt." {
		t.Fatalf("system = %q, want the unchanged base prompt", system)
	}

	builder = builder.WithPersona(func() agent.Persona { return agent.Persona{Name: "Empty", Prompt: "   "} })
	system = systemMessageOfPrompt(t, builder.Messages("hello", nil))
	if system != "Base prompt." {
		t.Fatalf("empty persona changed the system prompt to %q", system)
	}
}

// systemMessageOfPrompt reads the typed builder output through the same JSON
// wire shape the provider receives.
func systemMessageOfPrompt(t *testing.T, messages any) string {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(wire) == 0 {
		t.Fatal("builder returned no messages")
	}
	content, _ := wire[0]["content"].(string)
	if wire[0]["role"] != "system" {
		t.Fatalf("first message role = %v, want system", wire[0]["role"])
	}
	return content
}
