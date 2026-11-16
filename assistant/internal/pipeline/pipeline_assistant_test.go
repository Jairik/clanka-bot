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
	// The repeat Tool is Local-only for the Agent path, but the text it
	// repeats came from a public Tool, so the reply's provenance stays public
	// (#186). Otherwise "send that to my phone" would refuse a public answer.
	if repeated.LocalOnly {
		t.Error("repeat of a public reply is marked Local-only")
	}
	after, _ := store.Last()
	if after.Text != first.Text || after.Tool != assistant.RepeatName || after.LocalOnly {
		t.Fatalf("store after repeat = %+v, want the public repeat with provenance", after)
	}
}

// TestRepeatOfALocalOnlyReplyStaysLocalOnly is the other half of #186: the
// repeat follows the repeated text, so a private answer is still refused.
func TestRepeatOfALocalOnlyReplyStaysLocalOnly(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	store := lastreply.New()
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	addStringTool(t, reg, "test.private", "Your private list.", tool.PermissionRead, tool.LocalOnlyYes)
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"read the private one": "test.private",
		"say that again":       assistant.RepeatName,
	}, pipeline.WithLastReply(store))

	first := p.Handle(context.Background(), request(), "read the private one")
	if !first.LocalOnly {
		t.Fatalf("private reply = %+v, want Local-only", first)
	}
	repeated := p.Handle(context.Background(), request(), "say that again")
	if repeated.Text != first.Text || repeated.Tool != assistant.RepeatName || !repeated.LocalOnly {
		t.Fatalf("repeat reply = %+v, want the Local-only private text", repeated)
	}
	after, ok := store.Last()
	if !ok || !after.LocalOnly || after.Tool != assistant.RepeatName {
		t.Fatalf("store after repeat = %+v/%v, want the Local-only repeat", after, ok)
	}
}

// TestRepeatTurnReachesTheAgentHistoryOnlyWhenPublic checks the history side of
// #186: a public repeat is an ordinary turn the Agent may see, while a repeat
// of a Local-only reply keeps the turn and its utterance off the model request.
func TestRepeatTurnReachesTheAgentHistoryOnlyWhenPublic(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	store := lastreply.New()
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	addStringTool(t, reg, "test.private", "Your private list.", tool.PermissionRead, tool.LocalOnlyYes)
	runner := newFakeRunner()
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"what time is it":      clock.Name,
		"read the private one": "test.private",
		"say that again":       assistant.RepeatName,
	}, pipeline.WithAgent(runner, nil), pipeline.WithLastReply(store))

	p.Handle(context.Background(), request(), "what time is it")
	p.Handle(context.Background(), request(), "say that again")
	p.Handle(context.Background(), request(), "read the private one")
	p.Handle(context.Background(), request(), "say that again")

	turns := runner.History().Recent()
	if len(turns) != 4 {
		t.Fatalf("history has %d turns, want 4", len(turns))
	}
	if len(turns[1].Calls) != 1 || turns[1].Calls[0].LocalOnly {
		t.Errorf("public repeat turn = %+v, want one non-Local-only call", turns[1])
	}
	if len(turns[3].Calls) != 1 || !turns[3].Calls[0].LocalOnly {
		t.Errorf("private repeat turn = %+v, want one Local-only call", turns[3])
	}

	// The prompt builder is the only egress, so the public repeat must appear
	// there exactly once and the private text must not appear at all.
	messages := agent.NewPromptBuilder("").Messages("anything else", turns)
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	if got := strings.Count(string(encoded), "say that again"); got != 1 {
		t.Errorf("prompt has %d public repeat utterances, want 1: %s", got, encoded)
	}
	if strings.Contains(string(encoded), "Your private list.") {
		t.Errorf("prompt carries a Local-only result: %s", encoded)
	}
}

// TestChainOfRepeatedPublicReplyStaysPublic covers the chain path: a repeat
// followed by another confident step must not inherit the repeat Tool's static
// Local-only flag (#186).
func TestChainOfRepeatedPublicReplyStaysPublic(t *testing.T) {
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "It's 3:04 PM.", Tool: clock.Name})
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"say that again":  assistant.RepeatName,
		"what time is it": clock.Name,
	}, pipeline.WithLastReply(store))
	p.BindChains(func() bool { return true })

	reply := p.Handle(context.Background(), request(), "say that again and what time is it")
	if reply.Path != tool.PathFast || reply.Text != "It's 3:04 PM. It's 3:04 PM." {
		t.Fatalf("chain reply = %+v", reply)
	}
	if reply.LocalOnly {
		t.Error("chain reply with a public repeat is marked Local-only")
	}
	if got, _ := store.Last(); got.LocalOnly {
		t.Errorf("store after chain = %+v, want a public reply", got)
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
	// The spoken answer is grouped by area and bounded, so it names the areas
	// (here "assistant" and the test module) rather than repeating every
	// description, and it ends with the topic invitation. The complete Tool
	// list stays in HelpOut.Tools for the Dashboard (covered by the assistant
	// package's own test); only Text is capped.
	if !strings.Contains(reply.Text, "assistant") || !strings.Contains(reply.Text, "test") {
		t.Fatalf("help text does not name the areas: %q", reply.Text)
	}
	if !strings.HasSuffix(reply.Text, "Ask what I can do with one of them.") {
		t.Fatalf("help text does not end with the topic invitation: %q", reply.Text)
	}
	if len([]rune(reply.Text)) > 400 {
		t.Fatalf("help text exceeds the 400-rune budget: %q", reply.Text)
	}
	if !reply.LocalOnly {
		t.Error("help reply is not marked Local-only")
	}
}

// TestForgetControlPhraseClearsPriorContextBeforeTheAgent is the pipeline-level
// acceptance test: the Control phrase routes to assistant.forget, speaks the
// confirmation, and the stale context is gone from the history the Agent reads.
func TestForgetControlPhraseClearsPriorContextBeforeTheAgent(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runner := newFakeRunner()
	history := runner.History()
	if err := assistant.NewModule().Register(reg, tool.Deps{
		LastReply: lastreply.New(),
		History:   history,
	}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"forget that": assistant.ForgetName,
	}, pipeline.WithAgent(runner, nil), pipeline.WithHistory(history))

	history.Add(agent.Turn{Utterance: "my favorite is the blue one", Reply: "Noted."})

	reply := p.Handle(context.Background(), request(), "forget that")
	if reply.Path != tool.PathControlPhrase || reply.Tool != assistant.ForgetName {
		t.Fatalf("forget reply = %+v, want the forget Tool", reply)
	}
	if reply.Text != "Forgotten." {
		t.Fatalf("forget text = %q, want Forgotten.", reply.Text)
	}
	if !reply.LocalOnly {
		t.Error("forget reply is not Local-only")
	}
	assertNoStaleContext(t, history, "blue one")
}

// TestNonHistoryForgetDoesNotClear is the semantic-side regression: "forget my
// shopping list" is about a list, not the conversation, so it must fall through
// to the Agent path and leave the shared history intact. A genuine
// conversation forget on the same router clears it.
func TestNonHistoryForgetDoesNotClear(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runner := newFakeRunner()
	history := runner.History()
	// Register only the forget Tool so the fake embedder only needs vectors for
	// its Examples; the whole module would index repeat/help Examples too.
	if err := tool.Add(reg, assistant.NewForget(history)); err != nil {
		t.Fatalf("add forget: %v", err)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"forget that":                          {1, 0},
		"forget the conversation":              {1, 0},
		"clear our chat":                       {1, 0},
		"start over":                           {1, 0},
		"never mind that":                      {1, 0},
		"forget everything we've talked about": {1, 0},
		"let's start fresh":                    {1, 0},
		"clear the conversation history":       {1, 0},
		"forget my shopping list":              {0, 1},
	}}
	p, _ := newSemanticPipeline(t, reg, nil, emb,
		pipeline.WithAgent(runner, nil), pipeline.WithHistory(history))

	history.Add(agent.Turn{Utterance: "my favorite is the blue one", Reply: "Noted."})

	other := p.Handle(context.Background(), request(), "forget my shopping list")
	if other.Tool == assistant.ForgetName || other.Path == tool.PathFast {
		t.Fatalf("non-history forget misrouted to the forget Tool: %+v", other)
	}
	if turns := history.Recent(); len(turns) != 1 || turns[0].Utterance != "my favorite is the blue one" {
		t.Fatalf("history changed after a non-history forget: %+v", turns)
	}

	forgot := p.Handle(context.Background(), request(), "forget the conversation")
	if forgot.Tool != assistant.ForgetName || forgot.Text != "Forgotten." {
		t.Fatalf("forget reply = %+v, want the forget Tool confirmation", forgot)
	}
	assertNoStaleContext(t, history, "blue one")
}

// assertNoStaleContext checks both the stored turns and the prompt the next
// Agent request would build, since forgetting is only useful if stale context
// no longer reaches the model.
func assertNoStaleContext(t *testing.T, history *agent.History, stale string) {
	t.Helper()
	turns := history.Recent()
	for _, turn := range turns {
		if strings.Contains(turn.Utterance, stale) || strings.Contains(turn.Reply, stale) {
			t.Errorf("stale turn survived forget: %+v", turn)
		}
	}
	messages := agent.NewPromptBuilder("").Messages("anything else", turns)
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	if strings.Contains(string(encoded), stale) {
		t.Errorf("next Agent prompt still carries stale context %q: %s", stale, encoded)
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

// TestRepeatRateStepsAndResets covers the per-utterance replay rate through the
// whole pipeline: a slower step uses the previous applied rate (so two in a row
// step twice), a plain repeat keeps the setting, and the next ordinary reply is
// back at the setting's rate with no stored override.
func TestRepeatRateStepsAndResets(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	store := lastreply.New()
	// No Voices seam: NewRepeat falls back to the neutral 1.0 setting.
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"what time is it":       clock.Name,
		"say that again slower": assistant.RepeatName,
		"say that again":        assistant.RepeatName,
	}, pipeline.WithLastReply(store))

	first := p.Handle(context.Background(), request(), "what time is it")
	if first.SpeechRate != 0 {
		t.Fatalf("ordinary reply rate = %v, want 0 (the setting)", first.SpeechRate)
	}
	if got, _ := store.Last(); got.SpeechRate != 0 {
		t.Fatalf("stored rate after ordinary reply = %v, want 0", got.SpeechRate)
	}

	slow1 := p.Handle(context.Background(), request(), "say that again slower")
	if slow1.SpeechRate != 0.8 {
		t.Fatalf("first slower rate = %v, want 0.8 (one step from 1.0)", slow1.SpeechRate)
	}
	if got, _ := store.Last(); got.SpeechRate != 0.8 {
		t.Fatalf("stored rate after first slower = %v, want 0.8", got.SpeechRate)
	}

	// Two "slower" requests in a row step from the previously applied rate.
	slow2 := p.Handle(context.Background(), request(), "say that again slower")
	if slow2.SpeechRate != 0.64 {
		t.Fatalf("second slower rate = %v, want 0.64 (a second step from 0.8)", slow2.SpeechRate)
	}
	if slow2.Text != first.Text || slow2.LocalOnly {
		t.Fatalf("slower replay = %+v, want the same public text", slow2)
	}
	if got, _ := store.Last(); got.SpeechRate != 0.64 {
		t.Fatalf("stored rate after second slower = %v, want 0.64", got.SpeechRate)
	}

	// A plain repeat keeps the setting; it does not inherit the last override.
	plain := p.Handle(context.Background(), request(), "say that again")
	if plain.SpeechRate != 0 {
		t.Fatalf("plain repeat rate = %v, want 0", plain.SpeechRate)
	}
	if got, _ := store.Last(); got.SpeechRate != 0 {
		t.Fatalf("stored rate after plain repeat = %v, want 0", got.SpeechRate)
	}

	// The next ordinary reply is back at the setting's rate with nothing stored.
	next := p.Handle(context.Background(), request(), "what time is it")
	if next.SpeechRate != 0 {
		t.Fatalf("next ordinary reply rate = %v, want 0", next.SpeechRate)
	}
	if got, _ := store.Last(); got.SpeechRate != 0 {
		t.Fatalf("stored rate after next ordinary reply = %v, want 0", got.SpeechRate)
	}
}

// TestCancelledRepeatStoresNoRateOrReply proves a stopped turn cannot leave a
// replay rate (or a changed last reply) behind for the next request.
func TestCancelledRepeatStoresNoRateOrReply(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	store := lastreply.New()
	store.Record(lastreply.Reply{Text: "original answer", Tool: clock.Name})
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	p, _ := newComposedPipeline(t, reg, map[string]string{
		"say that again slower": assistant.RepeatName,
	}, pipeline.WithLastReply(store))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	reply := p.Handle(cancelled, request(), "say that again slower")
	if reply.SpeechRate != 0 {
		t.Fatalf("cancelled turn replay rate = %v, want 0 (no override)", reply.SpeechRate)
	}
	got, ok := store.Last()
	if !ok || got.Text != "original answer" || got.SpeechRate != 0 {
		t.Fatalf("store after cancelled repeat = %+v/%v, want the unchanged original", got, ok)
	}
}
