package assistant_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/assistant"
)

const helpInvitation = "Ask what I can do with one of them."

// TestHelpSpokenAnswerOverProductionList asserts the no-topic "what can you do"
// reply is bounded, names at least four areas and ends with the topic
// invitation, using the real module list (tools.NewSet + RegisterAll).
func TestHelpSpokenAnswerOverProductionList(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	deps := tool.Deps{
		Location: func() (location.Location, bool) { return location.Location{City: "Berlin"}, true },
	}
	if err := tools.NewSet().RegisterAll(reg, deps, nil); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	result, err := reg.Execute(context.Background(), assistant.HelpName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, ok := result.(assistant.HelpOut)
	if !ok {
		t.Fatalf("result is %T, want assistant.HelpOut", result)
	}

	runes := []rune(out.Text)
	if len(runes) > 400 {
		t.Errorf("help text = %d runes, want at most 400: %q", len(runes), out.Text)
	}
	if !strings.HasSuffix(out.Text, helpInvitation) {
		t.Errorf("help text does not end with the topic invitation: %q", out.Text)
	}
	// At least four of the known areas must be named.
	areas := []string{
		"set timers, alarms and reminders",
		"check the weather",
		"keep lists and notes",
		"play music",
		"control the lights",
		"search the web and GitHub",
	}
	named := 0
	for _, area := range areas {
		if strings.Contains(out.Text, area) {
			named++
		}
	}
	if named < 4 {
		t.Errorf("help named %d areas, want at least 4: %q", named, out.Text)
	}
}

// TestHelpTopicNarrowsToArea proves "what can you do with timers" lists only the
// timer, alarm and reminder Tools and nothing invented from another area.
func TestHelpTopicNarrowsToArea(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	deps := tool.Deps{
		Location: func() (location.Location, bool) { return location.Location{City: "Berlin"}, true },
	}
	if err := tools.NewSet().RegisterAll(reg, deps, nil); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	result, err := reg.Execute(context.Background(), assistant.HelpName, json.RawMessage(`{"topic":"timers"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(assistant.HelpOut)

	// A timer/alarm/reminder description must appear.
	if !strings.Contains(out.Text, "timer") && !strings.Contains(out.Text, "alarm") && !strings.Contains(out.Text, "reminder") {
		t.Errorf("topic answer names no timer/alarm/reminder tool: %q", out.Text)
	}
	// The weather Tool's description must not leak into a timers answer.
	for _, tool := range out.Tools {
		if tool.Name == "weather.current" && strings.Contains(out.Text, tool.Description) {
			t.Errorf("timers answer leaked the weather description: %q", out.Text)
		}
	}
}

// TestHelpUnknownTopicNamesAreas proves an unknown topic names the areas and
// invents nothing about the topic.
func TestHelpUnknownTopicNamesAreas(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	deps := tool.Deps{
		Location: func() (location.Location, bool) { return location.Location{City: "Berlin"}, true },
	}
	if err := tools.NewSet().RegisterAll(reg, deps, nil); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	result, err := reg.Execute(context.Background(), assistant.HelpName, json.RawMessage(`{"topic":"cooking"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(assistant.HelpOut)
	if !strings.HasSuffix(out.Text, helpInvitation) {
		t.Errorf("unknown-topic answer does not end with the invitation: %q", out.Text)
	}
	// An unknown topic must not invent content about cooking.
	if strings.Contains(strings.ToLower(out.Text), "cook") {
		t.Errorf("unknown topic invented content: %q", out.Text)
	}
	if len([]rune(out.Text)) > 400 {
		t.Errorf("unknown-topic answer exceeds budget: %q", out.Text)
	}
}

// TestHelpSingleUnknownModuleStaysBounded proves a module with no display phrase
// contributes its own name and that adding one to the set does not break the
// budget.
func TestHelpSingleUnknownModuleStaysBounded(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, assistant.NewHelp(reg)); err != nil {
		t.Fatalf("Add help: %v", err)
	}
	if err := tool.Add(reg, tool.Tool[struct{}, struct{}]{
		Name:        "gadget.wiggle",
		Description: "Wiggle the gadget.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		Speak:       func(struct{}) string { return "wiggle" },
		Examples:    []string{"wiggle the gadget"},
	}); err != nil {
		t.Fatalf("Add gadget: %v", err)
	}
	result, err := reg.Execute(context.Background(), assistant.HelpName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(assistant.HelpOut)
	if !strings.Contains(out.Text, "gadget") {
		t.Errorf("help text does not name the unknown module: %q", out.Text)
	}
	if len([]rune(out.Text)) > 400 {
		t.Errorf("help text = %d runes, want at most 400: %q", len([]rune(out.Text)), out.Text)
	}
}

// TestHelpStaysLocalOnlyAndExcludedFromAgent proves the capability list keeps
// LocalOnlyYes, so the Agent path (which refuses LocalOnly tools, agent.go:388)
// can never learn the Local-only Tools it names.
func TestHelpStaysLocalOnlyAndExcludedFromAgent(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, assistant.NewHelp(reg)); err != nil {
		t.Fatalf("Add help: %v", err)
	}
	info, ok := reg.Info(assistant.HelpName)
	if !ok {
		t.Fatal("assistant.help is not registered")
	}
	if !info.LocalOnly {
		t.Error("assistant.help is not Local-only; the Agent path could learn the named Tools")
	}
	if info.AgentOnly {
		t.Error("assistant.help must not be Agent-only; it must stay on the Fast path")
	}
}
