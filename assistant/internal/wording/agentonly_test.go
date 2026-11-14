package wording_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

func TestBuildRejectsAnAgentOnlyControlPhraseTarget(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[testIn, testOut]{
		Name:        "web.search",
		Description: "Agent-only test tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   true,
		Handler: func(context.Context, testIn) (testOut, error) {
			return testOut{Text: "result"}, nil
		},
		Speak: func(out testOut) string { return out.Text },
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := wording.Config{
		ControlPhrases:     map[string]string{"search the web": "web.search"},
		RequestPrefixes:    []string{"ask the agent"},
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	}
	_, err := wording.Build(reg, cfg)
	if err == nil {
		t.Fatal("Build accepted an AgentOnly Control-phrase target")
	}
	if !strings.Contains(err.Error(), "AgentOnly") {
		t.Fatalf("error = %v, want it to name the AgentOnly rule", err)
	}
}
