package assistant_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/tools/assistant"
)

func newRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry(time.Second)
}

func execute(t *testing.T, reg *tool.Registry, name string) any {
	t.Helper()
	result, err := reg.Execute(context.Background(), name, nil)
	if err != nil {
		t.Fatalf("Execute %s: %v", name, err)
	}
	return result
}

func TestRepeatSpeaksThePreviousCompletedReply(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{
		Text: "It's 3:04 PM.",
		Tool: "time.now",
		Path: "fast_path",
	})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	out, ok := execute(t, reg, assistant.RepeatName).(assistant.RepeatOut)
	if !ok {
		t.Fatal("result is not assistant.RepeatOut")
	}
	if out.Text != "It's 3:04 PM." || out.Tool != "time.now" || out.Path != "fast_path" {
		t.Fatalf("RepeatOut = %+v, want the recorded reply", out)
	}
	// The repeat must not echo the current request; the Tool has no input at
	// all, so there is nothing from "say that again" to read back.
	if spoken, _ := reg.Speak(assistant.RepeatName, out); spoken != "It's 3:04 PM." {
		t.Fatalf("Speak = %q, want the previous reply", spoken)
	}
}

func TestRepeatWithoutAPreviousReplyFailsClearly(t *testing.T) {
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(lastreply.New())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), assistant.RepeatName, nil)
	if err == nil {
		t.Fatal("Execute succeeded with no previous reply")
	}
	if got := tool.FailureMessage(err); got != "I haven't said anything yet." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

// TestHeardSpeaksTheTranscript covers assistant.heard: it reads back the
// transcript the Assistant acted on verbatim with its provenance.
func TestHeardSpeaksTheTranscript(t *testing.T) {
	fixed := time.Date(2026, 11, 12, 15, 4, 0, 0, time.UTC)
	store := lastreply.New()
	store.Record(lastreply.Reply{
		Text:      "It's 3:04 PM.",
		Tool:      "time.now",
		Path:      "fast_path",
		Utterance: "what time is it",
		Source:    "voice",
		At:        fixed,
	})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewHeard(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	out, ok := execute(t, reg, assistant.HeardName).(assistant.HeardOut)
	if !ok {
		t.Fatal("result is not assistant.HeardOut")
	}
	if out.Text != `I heard: "what time is it".` || out.Utterance != "what time is it" || out.Source != "voice" || !out.At.Equal(fixed) {
		t.Fatalf("HeardOut = %+v, want the recorded transcript and provenance", out)
	}
	if spoken, _ := reg.Speak(assistant.HeardName, out); spoken != `I heard: "what time is it".` {
		t.Fatalf("Speak = %q, want I heard: \"what time is it\".", spoken)
	}
}

// TestHeardWithoutAPreviousReplyFailsClearly asserts that an empty store fails
// with the expected spoken failure text.
func TestHeardWithoutAPreviousReplyFailsClearly(t *testing.T) {
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewHeard(lastreply.New())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), assistant.HeardName, nil)
	if err == nil {
		t.Fatal("Execute succeeded with no previous reply")
	}
	if got := tool.FailureMessage(err); got != "I haven't heard anything yet." {
		t.Fatalf("FailureMessage = %q, want I haven't heard anything yet.", got)
	}
}

// TestHeardWithBlankTranscriptFailsClearly asserts that a blank transcript
// also fails with "I haven't heard anything yet."
func TestHeardWithBlankTranscriptFailsClearly(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{
		Text:      "The complete answer.",
		Utterance: "   ",
	})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewHeard(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), assistant.HeardName, nil)
	if err == nil {
		t.Fatal("Execute succeeded with blank utterance")
	}
	if got := tool.FailureMessage(err); got != "I haven't heard anything yet." {
		t.Fatalf("FailureMessage = %q, want I haven't heard anything yet.", got)
	}
}

// TestHeardMetadata asserts Permission read, LocalOnlyYes, and that the Agent
// tool set excludes it.
func TestHeardMetadata(t *testing.T) {
	reg := newRegistry(t)
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: lastreply.New()}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	info, ok := reg.Info(assistant.HeardName)
	if !ok {
		t.Fatalf("info for %s not found", assistant.HeardName)
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
	if !info.LocalOnly {
		t.Error("assistant.heard is not Local-only; transcripts must never leave the Pi")
	}
	if info.AgentOnly {
		t.Error("assistant.heard is AgentOnly; it must run on the Fast path")
	}

	// The Agent path prompt builder must exclude assistant.heard.
	agentTools := agent.NewPromptBuilder("").Tools(reg)
	for _, at := range agentTools {
		if at.OfFunction != nil && at.OfFunction.Function.Name == info.WireName {
			t.Errorf("Agent tool set includes %s, want it excluded", info.WireName)
		}
	}
}

// TestForgetClearsHistoryAndKeepsLastReply covers the three core promises of
// assistant.forget: the module wiring passes the shared history to the Tool, a
// call clears that history, and the spoken confirmation says what happened.
// The last-reply store is deliberately left alone so "say that again" still
// works after a forget.
func TestForgetClearsHistoryAndKeepsLastReply(t *testing.T) {
	history := agent.NewHistory(10, 5*time.Minute, nil)
	history.Add(agent.Turn{Utterance: "remember the blue one", Reply: "Okay."})
	history.Add(agent.Turn{Utterance: "and the red one", Reply: "Got it."})

	replies := lastreply.New()
	replies.Record(lastreply.Reply{Text: "It's 3:04 PM.", Tool: "time.now", Path: "fast_path"})

	reg := newRegistry(t)
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: replies, History: history}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	out, ok := execute(t, reg, assistant.ForgetName).(assistant.ForgetOut)
	if !ok {
		t.Fatal("result is not assistant.ForgetOut")
	}
	if out.Text != "Forgotten." {
		t.Fatalf("ForgetOut.Text = %q, want Forgotten.", out.Text)
	}
	if spoken, ok := reg.Speak(assistant.ForgetName, out); !ok || spoken != "Forgotten." {
		t.Fatalf("Speak = %q/%v, want the spoken confirmation Forgotten.", spoken, ok)
	}
	if turns := history.Recent(); len(turns) != 0 {
		t.Fatalf("history after forget = %+v, want cleared", turns)
	}
	if reply, ok := replies.Last(); !ok || reply.Text != "It's 3:04 PM." {
		t.Fatalf("last reply after forget = %+v/%v, want it untouched", reply, ok)
	}

	info, _ := reg.Info(assistant.ForgetName)
	if info.Permission != tool.PermissionAct {
		t.Errorf("Permission = %q, want act", info.Permission)
	}
	if !info.LocalOnly {
		t.Error("assistant.forget is not Local-only; its result could leave the Pi")
	}
	if info.AgentOnly {
		t.Error("assistant.forget is AgentOnly; it must run on the Fast path without a provider")
	}
}

// TestForgetWithoutHistoryFailsClearly is the nil-seam regression: a caller
// with no shared history gets a spoken failure, never a panic.
func TestForgetWithoutHistoryFailsClearly(t *testing.T) {
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewForget(nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Execute(context.Background(), assistant.ForgetName, nil)
	if err == nil {
		t.Fatal("Execute succeeded with no history")
	}
	if got := tool.FailureMessage(err); got != "I can't forget anything right now." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

func TestRepeatKeepsTheLocalOnlyProvenance(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "Your alarms are ...", Tool: "alarm.list", LocalOnly: true})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := execute(t, reg, assistant.RepeatName).(assistant.RepeatOut)
	if !out.LocalOnly {
		t.Fatal("RepeatOut.LocalOnly = false, want the recorded Local-only provenance")
	}
	if !reg.Provenance(assistant.RepeatName, out) {
		t.Fatal("declared provenance = false, want the recorded Local-only flag")
	}

	info, _ := reg.Info(assistant.RepeatName)
	if !info.LocalOnly {
		t.Fatal("assistant.repeat is not Local-only; the Agent path could see a Local-only reply as Tool output")
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
	}
}

// TestRepeatReportsTheRepeatedTextsProvenance is the #186 unit seam: a repeat
// of a public reply reports public provenance even though the repeat Tool
// itself stays Local-only for the Agent path.
func TestRepeatReportsTheRepeatedTextsProvenance(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "It's 3:04 PM.", Tool: "time.now"})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(store)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := execute(t, reg, assistant.RepeatName).(assistant.RepeatOut)
	if reg.Provenance(assistant.RepeatName, out) {
		t.Fatal("declared provenance = true, want public provenance for a public reply")
	}
	info, _ := reg.Info(assistant.RepeatName)
	if !info.LocalOnly {
		t.Fatal("assistant.repeat is not Local-only; the Agent path could see a Local-only reply as Tool output")
	}
}

func TestHelpIsGeneratedFromTheEnabledRegistry(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(lastreply.New())); err != nil {
		t.Fatalf("Add repeat: %v", err)
	}
	if err := tool.Add(reg, assistant.NewHelp(reg)); err != nil {
		t.Fatalf("Add help: %v", err)
	}
	addTestTool(t, reg, "test.alpha", "Alpha does the first thing.")
	addTestTool(t, reg, "test.beta", "Beta does the second thing.")
	if err := tool.Add(reg, localOnlyTool("test.private", "Private reads on the Pi.")); err != nil {
		t.Fatalf("Add private: %v", err)
	}

	// The real settings service and tool policy supply the effective Enabled
	// flag, exactly as the Dashboard and voice overrides do in production.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	defs, err := toolpolicy.Definitions(reg, toolpolicy.Config{})
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if _, err := toolpolicy.New(svc, reg); err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}

	first := execute(t, reg, assistant.HelpName).(assistant.HelpOut)
	names := helpNames(first)
	for _, want := range []string{assistant.RepeatName, assistant.HelpName, "test.alpha", "test.beta", "test.private"} {
		if !names[want] {
			t.Errorf("help lacks enabled Tool %q: %+v", want, first.Tools)
		}
	}
	// The spoken answer is grouped and bounded, so it names the areas (here the
	// assistant and test modules) rather than repeating every description, and it
	// never exceeds the 400-rune budget or omits the topic invitation.
	if got := []rune(first.Text); len(got) > 400 {
		t.Errorf("help text = %d runes, want at most 400: %q", len(got), first.Text)
	}
	if !strings.HasSuffix(first.Text, "Ask what I can do with one of them.") {
		t.Errorf("help text does not end with the topic invitation: %q", first.Text)
	}
	for _, area := range []string{"assistant", "test"} {
		if !strings.Contains(first.Text, area) {
			t.Errorf("help text lacks area %q: %q", area, first.Text)
		}
	}

	// Disabling one Tool must remove it from the very next call without
	// re-registering the Tool or rebuilding the help result.
	if _, err := svc.Set(ctx, toolpolicy.EnabledKey("test.alpha"), false); err != nil {
		t.Fatalf("disable test.alpha: %v", err)
	}
	second := execute(t, reg, assistant.HelpName).(assistant.HelpOut)
	if helpNames(second)["test.alpha"] {
		t.Errorf("disabled test.alpha still appears: %+v", second.Tools)
	}
	// Disabling a Tool removes it from the Tool list but never from its area, so
	// the spoken text still names the area and stays bounded and honest.
	if helpNames(second)["test.alpha"] {
		t.Errorf("disabled test.alpha still appears: %+v", second.Tools)
	}
	if !helpNames(second)["test.beta"] {
		t.Errorf("enabled test.beta disappeared after disabling alpha: %+v", second.Tools)
	}
	if got := []rune(second.Text); len(got) > 400 {
		t.Errorf("second help text = %d runes, want at most 400: %q", len(got), second.Text)
	}
	if !strings.HasSuffix(second.Text, "Ask what I can do with one of them.") {
		t.Errorf("second help text does not end with the topic invitation: %q", second.Text)
	}

	info, _ := reg.Info(assistant.HelpName)
	if !info.LocalOnly {
		t.Fatal("assistant.help is not Local-only; it names Tools the Agent path must not see")
	}
}

// TestConcurrentRepeatHelpAndRecordAreRaceFree exercises the three shared
// pieces together; run with -race to prove the store's lock and the registry
// reads cover concurrent turns.
func TestConcurrentRepeatHelpAndRecordAreRaceFree(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "A previous reply.", Tool: "time.now"})
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewRepeat(store)); err != nil {
		t.Fatalf("Add repeat: %v", err)
	}
	if err := tool.Add(reg, assistant.NewHelp(reg)); err != nil {
		t.Fatalf("Add help: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			store.Record(lastreply.Reply{Text: fmt.Sprintf("reply %d", i)})
		}(i)
		go func() {
			defer wg.Done()
			if _, err := reg.Execute(context.Background(), assistant.RepeatName, nil); err != nil {
				t.Errorf("repeat: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := reg.Execute(context.Background(), assistant.HelpName, nil); err != nil {
				t.Errorf("help: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestHelpWithNoRegistryIsHonest(t *testing.T) {
	reg := newRegistry(t)
	if err := tool.Add(reg, assistant.NewHelp(nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := execute(t, reg, assistant.HelpName).(assistant.HelpOut)
	if got := out.Text; got != "I don't have any tools enabled right now." {
		t.Fatalf("Text = %q", got)
	}
	if len(out.Tools) != 0 {
		t.Fatalf("Tools = %+v, want none", out.Tools)
	}
}

func addTestTool(t *testing.T, reg *tool.Registry, name, description string) {
	t.Helper()
	err := tool.Add(reg, tool.Tool[struct{}, struct{}]{
		Name:        name,
		Description: description,
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		Speak:       func(struct{}) string { return description },
		Examples:    []string{description},
	})
	if err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

func localOnlyTool(name, description string) tool.Tool[struct{}, struct{}] {
	return tool.Tool[struct{}, struct{}]{
		Name:        name,
		Description: description,
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler:     func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		Speak:       func(struct{}) string { return description },
		Examples:    []string{description},
	}
}

func helpNames(out assistant.HelpOut) map[string]bool {
	names := make(map[string]bool, len(out.Tools))
	for _, item := range out.Tools {
		names[item.Name] = true
	}
	return names
}
