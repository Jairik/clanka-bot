package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
)

// systemOf decodes the one system message the builder produces.
func systemOf(t *testing.T, messages []openai.ChatCompletionMessageParamUnion) string {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	var decoded []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(decoded) == 0 || decoded[0].Role != "system" {
		t.Fatalf("first message = %+v, want one system part", decoded)
	}
	return decoded[0].Content
}

// TestDefaultPromptBansChattyRepliesAndNamesTheIgnoreSignal pins the rules the
// owner asked for after the measured Agent output (#496/#498).
func TestDefaultPromptBansChattyRepliesAndNamesTheIgnoreSignal(t *testing.T) {
	for _, want := range []string{
		"one short sentence",
		"lists",
		"markdown",
		"emoji",
		"capabilities",
		"Tools",
		"earlier topic",
		agent.IgnoreToken,
		"background conversation",
		"garbled transcription",
	} {
		if !strings.Contains(agent.DefaultSystemPrompt, want) {
			t.Errorf("DefaultSystemPrompt is missing %q", want)
		}
	}
}

// TestClarifyRuleUsesTheLiveLineAndPrecedesThePersona proves the Clarify line is
// read per request and that a Persona cannot replace it: the rule is part of the
// base system part and the Persona is appended after it.
func TestClarifyRuleUsesTheLiveLineAndPrecedesThePersona(t *testing.T) {
	line := "Say that again."
	builder := agent.NewPromptBuilder("").
		WithClarify(func() string { return line }).
		WithPersona(func() agent.Persona {
			return agent.Persona{Name: "Aria", Prompt: "Speak like a lighthouse keeper."}
		})

	system := systemOf(t, builder.Messages("hello", nil))
	if !strings.Contains(system, `"Say that again."`) {
		t.Fatalf("system part lacks the clarify line:\n%s", system)
	}
	if !strings.Contains(system, "cannot tell what is being asked") {
		t.Fatalf("system part lacks the clarify rule:\n%s", system)
	}
	clarify := strings.Index(system, "cannot tell what is being asked")
	persona := strings.Index(system, "Persona instructions for \"Aria\":")
	if clarify < 0 || persona < 0 || clarify > persona {
		t.Fatalf("clarify rule must precede the Persona:\n%s", system)
	}

	// A change applies to the next request without rebuilding the builder.
	line = "One more time, please."
	if next := systemOf(t, builder.Messages("hello", nil)); !strings.Contains(next, `"One more time, please."`) {
		t.Fatalf("system part kept the old clarify line:\n%s", next)
	}
}

// TestPlainBuilderKeepsTheOldPromptShape keeps a caller that never wires the
// Clarify source byte-for-byte unchanged.
func TestPlainBuilderKeepsTheOldPromptShape(t *testing.T) {
	system := systemOf(t, agent.NewPromptBuilder("Be brief.").Messages("hello", nil))
	if system != "Be brief." {
		t.Fatalf("system = %q, want the plain prompt", system)
	}
}
