package assistant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/assistant"
)

// fakeQueue provides stubbed announcement lines for testing.
type fakeQueue struct {
	lines []announce.Line
}

// Recent returns the canned lines.
func (f *fakeQueue) Recent() []announce.Line {
	return append([]announce.Line(nil), f.lines...)
}

// TestAnnouncementsEmpty proves an empty ring returns "Nothing." and invents no content.
func TestAnnouncementsEmpty(t *testing.T) {
	q := &fakeQueue{lines: nil}
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewAnnouncements(q)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	out, ok := execute(t, reg, assistant.AnnouncementsName).(assistant.AnnouncementsOut)
	if !ok {
		t.Fatal("result is not assistant.AnnouncementsOut")
	}
	if out.Text != "Nothing." {
		t.Errorf("Text = %q, want Nothing.", out.Text)
	}
	if spoken, ok := reg.Speak(assistant.AnnouncementsName, out); !ok || spoken != "Nothing." {
		t.Errorf("Speak = %q/%v, want Nothing.", spoken, ok)
	}
}

// TestAnnouncementsOne proves a single line renders with its relative time and text.
func TestAnnouncementsOne(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 10, 0, 0, time.UTC)
	q := &fakeQueue{
		lines: []announce.Line{
			{
				Text:       "Dinner is ready.",
				EnqueuedAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
				Kind:       announce.KindGeneric,
				State:      announce.StateSpoken,
			},
		},
	}
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewAnnouncements(q, func() time.Time { return now })); err != nil {
		t.Fatalf("Add: %v", err)
	}

	out, ok := execute(t, reg, assistant.AnnouncementsName).(assistant.AnnouncementsOut)
	if !ok {
		t.Fatal("result is not assistant.AnnouncementsOut")
	}
	want := "Ten minutes ago: Dinner is ready."
	if out.Text != want {
		t.Errorf("Text = %q, want %q", out.Text, want)
	}
	if spoken, ok := reg.Speak(assistant.AnnouncementsName, out); !ok || spoken != want {
		t.Errorf("Speak = %q/%v, want %q", spoken, ok, want)
	}
}

// TestAnnouncementsFive proves five lines are spoken oldest-first with relative time.
func TestAnnouncementsFive(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 30, 0, 0, time.UTC)
	q := &fakeQueue{
		lines: []announce.Line{
			{Text: "Line one.", EnqueuedAt: now.Add(-20 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line two.", EnqueuedAt: now.Add(-15 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line three.", EnqueuedAt: now.Add(-10 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line four.", EnqueuedAt: now.Add(-5 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line five.", EnqueuedAt: now.Add(-1 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
		},
	}
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewAnnouncements(q, func() time.Time { return now })); err != nil {
		t.Fatalf("Add: %v", err)
	}

	out, ok := execute(t, reg, assistant.AnnouncementsName).(assistant.AnnouncementsOut)
	if !ok {
		t.Fatal("result is not assistant.AnnouncementsOut")
	}
	want := "Twenty minutes ago: Line one. Fifteen minutes ago: Line two. Ten minutes ago: Line three. Five minutes ago: Line four. One minute ago: Line five."
	if out.Text != want {
		t.Errorf("Text = %q, want %q", out.Text, want)
	}
	if spoken, ok := reg.Speak(assistant.AnnouncementsName, out); !ok || spoken != want {
		t.Errorf("Speak = %q/%v, want %q", spoken, ok, want)
	}
}

// TestAnnouncementsMoreThanFive proves lines beyond five are capped with "and N more".
func TestAnnouncementsMoreThanFive(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 30, 0, 0, time.UTC)
	q := &fakeQueue{
		lines: []announce.Line{
			{Text: "Line one.", EnqueuedAt: now.Add(-25 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line two.", EnqueuedAt: now.Add(-20 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line three.", EnqueuedAt: now.Add(-15 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line four.", EnqueuedAt: now.Add(-10 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line five.", EnqueuedAt: now.Add(-5 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line six.", EnqueuedAt: now.Add(-2 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
			{Text: "Line seven.", EnqueuedAt: now.Add(-1 * time.Minute), Kind: announce.KindGeneric, State: announce.StateSpoken},
		},
	}
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewAnnouncements(q, func() time.Time { return now })); err != nil {
		t.Fatalf("Add: %v", err)
	}

	out, ok := execute(t, reg, assistant.AnnouncementsName).(assistant.AnnouncementsOut)
	if !ok {
		t.Fatal("result is not assistant.AnnouncementsOut")
	}
	if !strings.Contains(out.Text, "and 2 more") {
		t.Errorf("Text = %q, want it to contain 'and 2 more'", out.Text)
	}
	wantPrefix := "Twenty-five minutes ago: Line one."
	if !strings.HasPrefix(out.Text, wantPrefix) {
		t.Errorf("Text = %q, want prefix %q", out.Text, wantPrefix)
	}
	if strings.Contains(out.Text, "Line six") || strings.Contains(out.Text, "Line seven") {
		t.Errorf("Text contains 6th/7th line text, want capped at 5: %q", out.Text)
	}
}

// TestAnnouncementsNilQueueFailsClearly proves a nil queue fails cleanly with a spoken message.
func TestAnnouncementsNilQueueFailsClearly(t *testing.T) {
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewAnnouncements(nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := reg.Execute(context.Background(), assistant.AnnouncementsName, nil)
	if err == nil {
		t.Fatal("Execute succeeded with nil queue")
	}
	if got := tool.FailureMessage(err); got != "I can't check that right now." {
		t.Errorf("FailureMessage = %q, want 'I can't check that right now.'", got)
	}
}

// TestAnnouncementsMetadataLocalOnlyAndAbsentFromAgent proves the Tool is declared
// Local-only with PermissionRead, and is not offered in the Agent tool set.
func TestAnnouncementsMetadataLocalOnlyAndAbsentFromAgent(t *testing.T) {
	reg := newRegistry(t)
	q := &fakeQueue{}
	if err := assistant.NewModule().Register(reg, tool.Deps{Announcements: (*announce.Queue)(nil)}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = q

	info, ok := reg.Info(assistant.AnnouncementsName)
	if !ok {
		t.Fatalf("%s not registered", assistant.AnnouncementsName)
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
	if !info.LocalOnly {
		t.Errorf("%s is not Local-only", assistant.AnnouncementsName)
	}
	if info.AgentOnly {
		t.Errorf("%s is declared AgentOnly; it must be eligible for Fast path", assistant.AnnouncementsName)
	}

	// Assert absent from Agent tool set.
	builder := agent.NewPromptBuilder(agent.DefaultSystemPrompt)
	offered := builder.Tools(reg)
	for _, toolParam := range offered {
		if toolParam.OfFunction != nil && toolParam.OfFunction.Function.Name == info.WireName {
			t.Errorf("%s (%s) is offered in the Agent tool set; Local-only Tools must be absent",
				assistant.AnnouncementsName, info.WireName)
		}
	}
}
