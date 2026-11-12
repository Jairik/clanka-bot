package assistant_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

	info, _ := reg.Info(assistant.RepeatName)
	if !info.LocalOnly {
		t.Fatal("assistant.repeat is not Local-only; the Agent path could see a Local-only reply as Tool output")
	}
	if info.Permission != tool.PermissionRead {
		t.Errorf("Permission = %q, want read", info.Permission)
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
	for _, want := range []string{"Alpha does the first thing.", "Beta does the second thing.", "Private reads on the Pi."} {
		if !strings.Contains(first.Text, want) {
			t.Errorf("help text lacks %q: %q", want, first.Text)
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
	if strings.Contains(second.Text, "Alpha does the first thing.") {
		t.Errorf("disabled test.alpha description still spoken: %q", second.Text)
	}
	if !helpNames(second)["test.beta"] || !strings.Contains(second.Text, "Beta does the second thing.") {
		t.Errorf("enabled test.beta disappeared after disabling alpha: %+v", second.Tools)
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
