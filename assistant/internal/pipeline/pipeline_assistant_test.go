package pipeline_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/assistant"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/system"
)

func addStringTool(t *testing.T, reg *tool.Registry, name, reply string, permission tool.Permission, localOnly tool.LocalOnly) {
	t.Helper()
	err := tool.Add(reg, tool.Tool[struct{}, string]{
		Name:        name,
		Description: "Test Tool " + name + ".",
		Permission:  permission,
		LocalOnly:   localOnly,
		Handler:     func(context.Context, struct{}) (string, error) { return reply, nil },
		Speak:       func(out string) string { return out },
	})
	if err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

// TestRepeatRepeatsTheLastPipelineReply covers the whole spoken seam without
// the real embed service: a normal reply, then the repeat control target. The
// repeat reads the last-reply store, not the current utterance.
func TestRepeatRepeatsTheLastPipelineReply(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	store := lastreply.New()
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"what time is it": clock.Name,
		"say that again":  assistant.RepeatName,
	}, pipeline.WithLastReply(store))

	first := p.Handle(context.Background(), request(), "what time is it")
	if first.Text != "It's 3:04 PM." {
		t.Fatalf("first reply = %q", first.Text)
	}
	recorded, ok := store.Last()
	if !ok || recorded.Text != first.Text || recorded.Tool != clock.Name {
		t.Fatalf("last reply = %+v/%v, want the clock reply", recorded, ok)
	}

	repeated := p.Handle(context.Background(), request(), "say that again")
	if repeated.Path != tool.PathControlPhrase || repeated.Tool != assistant.RepeatName {
		t.Fatalf("repeat reply = %+v, want the repeat Tool", repeated)
	}
	if repeated.Text != first.Text {
		t.Fatalf("repeat text = %q, want %q", repeated.Text, first.Text)
	}
	if !repeated.LocalOnly {
		t.Error("repeat reply is not marked Local-only")
	}
	after, _ := store.Last()
	if after.Text != first.Text || after.Tool != assistant.RepeatName || !after.LocalOnly {
		t.Fatalf("store after repeat = %+v, want the repeat with provenance", after)
	}
}

func TestHelpListsTheEnabledRegistryAndDropsADisabledTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := assistant.NewModule().Register(reg, tool.Deps{}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	addStringTool(t, reg, "test.visible", "Visible reply.", tool.PermissionRead, tool.LocalOnlyNo)
	addStringTool(t, reg, "test.hidden", "Hidden reply.", tool.PermissionRead, tool.LocalOnlyNo)
	p := newPipeline(t, reg, map[string]string{"what can you do": assistant.HelpName}, mustLogger(t))

	reply := p.Handle(context.Background(), request(), "what can you do")
	if reply.Path != tool.PathControlPhrase || reply.Tool != assistant.HelpName {
		t.Fatalf("help reply = %+v", reply)
	}
	if !strings.Contains(reply.Text, "Test Tool test.visible.") || !strings.Contains(reply.Text, "Test Tool test.hidden.") {
		t.Fatalf("help text lacks enabled Tool descriptions: %q", reply.Text)
	}
	if !reply.LocalOnly {
		t.Error("help reply is not marked Local-only")
	}
}

func TestStatusSummarizesTheSharedHealthSeam(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := system.NewModule().Register(reg, tool.Deps{Status: func(context.Context) tool.StatusReport {
		return tool.StatusReport{
			Services: []tool.StatusCheck{
				{Name: "embed", State: tool.StatusOK},
				{Name: "tts", State: tool.StatusTimedOut},
			},
			Modules: []tool.StatusCheck{{Name: "weather", State: tool.StatusError}},
		}
	}}); err != nil {
		t.Fatalf("register system: %v", err)
	}
	p := newPipeline(t, reg, map[string]string{"are you ok": system.Name}, mustLogger(t))

	reply := p.Handle(context.Background(), request(), "are you ok")
	if reply.Path != tool.PathControlPhrase || reply.Tool != system.Name {
		t.Fatalf("status reply = %+v", reply)
	}
	if !strings.Contains(reply.Text, "tts") || !strings.Contains(reply.Text, "weather") {
		t.Fatalf("status text = %q, want the failing checks", reply.Text)
	}
	if strings.Contains(reply.Text, "I'm OK") {
		t.Fatalf("status text claims health with failing checks: %q", reply.Text)
	}
}

func TestLastReplyRecordsCompleteErrorAndCancelSensibly(t *testing.T) {
	store := lastreply.New()
	reg := tool.NewRegistry(time.Second)
	addStringTool(t, reg, "test.complete", "The complete answer.", tool.PermissionRead, tool.LocalOnlyNo)
	addStringTool(t, reg, "test.blank", "", tool.PermissionRead, tool.LocalOnlyNo)
	if err := tool.Add(reg, tool.Tool[struct{}, string]{
		Name:        "test.fail",
		Description: "Fail on purpose.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			return "", &tool.ToolError{Say: "That failed on purpose."}
		},
		Speak: func(string) string { return "" },
	}); err != nil {
		t.Fatalf("add failing tool: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"complete": "test.complete",
		"blank":    "test.blank",
		"fail":     "test.fail",
	}, pipeline.WithLastReply(store))

	// A complete reply is recorded.
	p.Handle(context.Background(), request(), "complete")
	if got, _ := store.Last(); got.Text != "The complete answer." {
		t.Fatalf("last after complete = %+v", got)
	}

	// A blank reply does not erase it.
	p.Handle(context.Background(), request(), "blank")
	if got, _ := store.Last(); got.Text != "The complete answer." {
		t.Fatalf("last after blank = %+v", got)
	}

	// An error the user heard is recorded; it is the last actual reply.
	p.Handle(context.Background(), request(), "fail")
	if got, _ := store.Last(); got.Text != "That failed on purpose." {
		t.Fatalf("last after error = %+v", got)
	}

	// A cancelled turn (Barge-in or shutdown) does not replace it.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	p.Handle(cancelled, request(), "complete")
	if got, _ := store.Last(); got.Text != "That failed on purpose." {
		t.Fatalf("last after cancellation = %+v, want the previous reply", got)
	}
}

func TestLastReplyKeepsLocalOnlyProvenance(t *testing.T) {
	store := lastreply.New()
	reg := tool.NewRegistry(time.Second)
	addStringTool(t, reg, "test.private", "Your private list.", tool.PermissionRead, tool.LocalOnlyYes)
	addStringTool(t, reg, "test.public", "A public answer.", tool.PermissionRead, tool.LocalOnlyNo)
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"private": "test.private",
		"public":  "test.public",
	}, pipeline.WithLastReply(store))

	private := p.Handle(context.Background(), request(), "private")
	if !private.LocalOnly {
		t.Fatal("Fast path reply from a Local-only Tool is not marked Local-only")
	}
	if got, _ := store.Last(); !got.LocalOnly || got.Tool != "test.private" {
		t.Fatalf("last private reply = %+v", got)
	}

	public := p.Handle(context.Background(), request(), "public")
	if public.LocalOnly {
		t.Fatal("Fast path reply from a public Tool is marked Local-only")
	}
	if got, _ := store.Last(); got.LocalOnly {
		t.Fatalf("last public reply = %+v", got)
	}
}

func TestLastReplyKeepsLocalOnlyProvenanceThroughAConfirmation(t *testing.T) {
	store := lastreply.New()
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[struct{}, string]{
		Name:        "test.confirm",
		Description: "Confirm a private action.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyYes,
		Handler:     func(context.Context, struct{}) (string, error) { return "Confirmed private.", nil },
		Speak:       func(out string) string { return out },
	}); err != nil {
		t.Fatalf("add confirm tool: %v", err)
	}
	if _, err := reg.Propose("test.confirm", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	logger := mustLogger(t)
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
		pipeline.WithLastReply(store),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "yes")
	if reply.Text != "Confirmed private." || !reply.LocalOnly {
		t.Fatalf("confirmation reply = %+v", reply)
	}
	got, ok := store.Last()
	if !ok || !got.LocalOnly || got.Text != "Confirmed private." {
		t.Fatalf("last reply after confirmation = %+v/%v", got, ok)
	}
}

func TestLastReplyRecordsTheAgentPathAsNotLocalOnly(t *testing.T) {
	store := lastreply.New()
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "A model answer."}
	logger := mustLogger(t)
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithLastReply(store),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	p.Handle(context.Background(), request(), "anything at all")
	got, ok := store.Last()
	if !ok || got.Text != "A model answer." || got.LocalOnly {
		t.Fatalf("last Agent reply = %+v/%v", got, ok)
	}
}

func mustLogger(t *testing.T) *slog.Logger {
	t.Helper()
	logger, _ := testLogger()
	return logger
}
