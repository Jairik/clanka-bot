package pipeline_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// confirmingRunner stands in for the Agent path: a model tool call arrives as
// a registry Propose, exactly as the real loop does.
type confirmingRunner struct {
	mu      sync.Mutex
	reg     *tool.Registry
	history *agent.History
	calls   []string
}

func newConfirmingRunner(reg *tool.Registry) *confirmingRunner {
	return &confirmingRunner{reg: reg, history: agent.NewHistory(10, 5*time.Minute, nil)}
}

func (c *confirmingRunner) Run(_ context.Context, _ tool.Request, utterance string, _ func(string)) agent.Result {
	c.mu.Lock()
	c.calls = append(c.calls, utterance)
	c.mu.Unlock()
	if utterance == "email my doctor" {
		pending, err := c.reg.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`))
		if err != nil {
			return agent.Result{Text: "could not propose"}
		}
		return agent.Result{Text: pending.Summary, Pending: &pending}
	}
	return agent.Result{Text: "I can answer that."}
}

func (c *confirmingRunner) History() *agent.History { return c.history }

func (c *confirmingRunner) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// addSendTool registers the test-only confirm Tool issue #24 uses.
func addSendTool(t *testing.T, reg *tool.Registry, runs *int) {
	t.Helper()
	err := tool.Add(reg, tool.Tool[sendIn, sendOut]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Summary:     func(in sendIn) string { return "Send the email to " + in.To + "?" },
		Handler: func(context.Context, sendIn) (sendOut, error) {
			*runs++
			return sendOut{Status: "sent"}, nil
		},
		Speak: func(out sendOut) string { return "Email " + out.Status + "." },
	})
	if err != nil {
		t.Fatalf("register mail.send: %v", err)
	}
}

type sendIn struct {
	To string `json:"to"`
}

type sendOut struct {
	Status string `json:"status"`
}

func newConfirmingPipeline(t *testing.T, reg *tool.Registry, runner *confirmingRunner, phrases map[string]string) *pipeline.Pipeline {
	t.Helper()
	logger, _ := testLogger()
	p, err := pipeline.New(reg, phrases, logger,
		pipeline.WithAgent(runner, []string{"ask the agent", "agent"}),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// askForConfirmation sends the request that makes the model call the confirm
// Tool, then checks the question was asked and nothing ran.
func askForConfirmation(t *testing.T, p *pipeline.Pipeline, runner *confirmingRunner, runs *int) {
	t.Helper()
	reply := p.Handle(context.Background(), request(), "email my doctor")
	if reply.Text != "Send the email to doctor?" {
		t.Fatalf("question = %q, want the registry summary", reply.Text)
	}
	if reply.Path != tool.PathAgent {
		t.Errorf("path = %q, want agent", reply.Path)
	}
	if *runs != 0 {
		t.Fatalf("confirm handler ran %d times before the yes", *runs)
	}
	if calls := runner.seen(); len(calls) != 1 || calls[0] != "email my doctor" {
		t.Fatalf("agent saw %v", calls)
	}
}

func TestConfirmToolAsksThenClearYesRunsIt(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)
	runner := newConfirmingRunner(reg)
	p := newConfirmingPipeline(t, reg, runner, nil)

	askForConfirmation(t, p, runner, &runs)
	if _, ok := reg.Pending(); !ok {
		t.Fatal("the model's confirm call was not armed in the registry")
	}

	reply := p.Handle(context.Background(), request(), "yes")
	if runs != 1 {
		t.Fatalf("confirm handler ran %d times, want 1 after the yes", runs)
	}
	if reply.Text != "Email sent." {
		t.Errorf("reply = %q, want the Speak rendering", reply.Text)
	}
	if reply.Path != tool.PathConfirmation || reply.Tool != "mail.send" {
		t.Errorf("path/tool = %q/%q, want confirmation/mail.send", reply.Path, reply.Tool)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("pending survived the yes")
	}
	if calls := runner.seen(); len(calls) != 1 {
		t.Errorf("agent saw %v, want the yes to stay out of the Agent path", calls)
	}

	turns := runner.History().Recent()
	if len(turns) != 1 || len(turns[0].Calls) != 1 || turns[0].Calls[0].Name != "mail.send" {
		t.Fatalf("history = %+v, want the confirmed call recorded", turns)
	}
}

func TestNoCancelsAndRoutesTheUtteranceNormally(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)
	runner := newConfirmingRunner(reg)
	p := newConfirmingPipeline(t, reg, runner, nil)

	askForConfirmation(t, p, runner, &runs)

	reply := p.Handle(context.Background(), request(), "no")
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times after a no", runs)
	}
	if !strings.HasPrefix(reply.Text, "Cancelled.") {
		t.Fatalf("reply = %q, want it to start with Cancelled.", reply.Text)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("pending survived a no")
	}
	calls := runner.seen()
	if len(calls) != 2 || calls[1] != "no" {
		t.Fatalf("agent saw %v, want the no routed normally after cancellation", calls)
	}
}

func TestSilenceCancelsWithoutRoutingOrRunning(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)
	runner := newConfirmingRunner(reg)
	p := newConfirmingPipeline(t, reg, runner, nil)

	askForConfirmation(t, p, runner, &runs)

	reply := p.Handle(context.Background(), request(), "  ")
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times on silence", runs)
	}
	if reply.Text != "Cancelled." {
		t.Errorf("reply = %q, want Cancelled.", reply.Text)
	}
	if reply.Path != tool.PathConfirmation {
		t.Errorf("path = %q, want confirmation", reply.Path)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("pending survived silence")
	}
	if calls := runner.seen(); len(calls) != 1 {
		t.Errorf("agent saw %v, want silence not routed as a request", calls)
	}
}

func TestUnrelatedRequestCancelsThenIsHandledNormally(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	runner := newConfirmingRunner(reg)
	p := newConfirmingPipeline(t, reg, runner, map[string]string{"what time is it": clock.Name})

	askForConfirmation(t, p, runner, &runs)

	reply := p.Handle(context.Background(), request(), "what time is it")
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times", runs)
	}
	if got, want := reply.Text, "Cancelled. It's 3:04 PM."; got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
	if reply.Path != tool.PathControlPhrase {
		t.Errorf("path = %q, want the unrelated request to keep its own path", reply.Path)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("pending survived an unrelated request")
	}
}

func TestControlPhraseCannotTargetAConfirmTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)

	logger, _ := testLogger()
	if _, err := pipeline.New(reg, map[string]string{"send it": "mail.send"}, logger); err == nil {
		t.Fatal("pipeline.New accepted a Control phrase pointing at a confirm Tool")
	}
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times, want 0", runs)
	}
}

func TestBargeInDropPendingSeamDropsAWaitingCall(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)
	runner := newConfirmingRunner(reg)
	p := newConfirmingPipeline(t, reg, runner, nil)

	askForConfirmation(t, p, runner, &runs)

	dropped, ok := p.DropPending()
	if !ok || dropped.Tool != "mail.send" {
		t.Fatalf("DropPending = %+v, %v, want mail.send", dropped, ok)
	}
	reply := p.Handle(context.Background(), request(), "yes")
	if runs != 0 {
		t.Fatalf("a dropped confirmation still ran %d times", runs)
	}
	if strings.HasPrefix(reply.Text, "Cancelled.") {
		t.Errorf("reply = %q, want no cancellation once the call was already dropped", reply.Text)
	}
	if reply.Path != tool.PathAgent {
		t.Errorf("path = %q, want the yes routed normally with no pending call", reply.Path)
	}
}
