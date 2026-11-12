package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// fakeRunner stands in for the Agent path at the pipeline seam.
type fakeRunner struct {
	mu         sync.Mutex
	utterances []string
	history    *agent.History
	result     agent.Result
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{history: agent.NewHistory(10, 5*time.Minute, nil)}
}

func (f *fakeRunner) Run(_ context.Context, _ tool.Request, utterance string, onDelta func(string)) agent.Result {
	f.mu.Lock()
	f.utterances = append(f.utterances, utterance)
	f.mu.Unlock()
	if onDelta != nil {
		onDelta("partial")
	}
	return f.result
}

func (f *fakeRunner) History() *agent.History { return f.history }

func (f *fakeRunner) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.utterances...)
}

func newAgentPipeline(t *testing.T, runner pipeline.AgentRunner, reg *tool.Registry, phrases map[string]string) *pipeline.Pipeline {
	t.Helper()
	logger, _ := testLogger()
	p, err := pipeline.New(reg, phrases, logger, pipeline.WithAgent(runner, []string{"ask the agent", "agent", "hey agent"}))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

func TestAgentRequestPrefixGoesStraightToTheAgent(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "It's sunny."}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	reply := p.Handle(context.Background(), request(), "ask the agent what's the weather")
	if reply.Text != "It's sunny." {
		t.Fatalf("reply = %q", reply.Text)
	}
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want agent", reply.Path)
	}
	if calls := runner.calls(); len(calls) != 1 || calls[0] != "what's the weather" {
		t.Fatalf("agent saw %v, want the prefix stripped", calls)
	}
}

func TestAgentPrefixWithoutRequestAsksWhatToAsk(t *testing.T) {
	runner := newFakeRunner()
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	reply := p.Handle(context.Background(), request(), "hey agent")
	if reply.Text != agent.EmptyRequestReply {
		t.Fatalf("reply = %q, want %q", reply.Text, agent.EmptyRequestReply)
	}
	if calls := runner.calls(); len(calls) != 0 {
		t.Fatalf("agent was called with %v, want no model call", calls)
	}
}

func TestControlPhraseStillTakesTheFastPathWithTheAgentConfigured(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	runner := newFakeRunner()
	p := newAgentPipeline(t, runner, reg, map[string]string{"what time is it": clock.Name})

	reply := p.Handle(context.Background(), request(), "what time is it")
	if reply.Path != tool.PathControlPhrase {
		t.Fatalf("path = %q, want control phrase", reply.Path)
	}
	if reply.Text != "It's 3:04 PM." {
		t.Fatalf("reply = %q", reply.Text)
	}
	if calls := runner.calls(); len(calls) != 0 {
		t.Fatalf("agent was called %v, want the Fast path", calls)
	}
}

func TestFastPathTurnJoinsTheSharedHistory(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	runner := newFakeRunner()
	p := newAgentPipeline(t, runner, reg, map[string]string{"what time is it": clock.Name})

	p.Handle(context.Background(), request(), "what time is it")

	turns := runner.History().Recent()
	if len(turns) != 1 {
		t.Fatalf("turns = %+v, want one shared turn", turns)
	}
	if turns[0].Utterance != "what time is it" || turns[0].Reply != "It's 3:04 PM." {
		t.Fatalf("turn = %+v", turns[0])
	}
	if len(turns[0].Calls) != 1 || turns[0].Calls[0].Name != clock.Name {
		t.Fatalf("turn calls = %+v, want the time.now call", turns[0].Calls)
	}
}

func TestOpenQuestionFallsThroughToTheAgent(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Here is an answer."}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	reply := p.Handle(context.Background(), request(), "why is the sky blue")
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want agent", reply.Path)
	}
	if calls := runner.calls(); len(calls) != 1 || calls[0] != "why is the sky blue" {
		t.Fatalf("agent saw %v", calls)
	}
}

func TestHandleEventsForwardsDeltasToTheAgent(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "partial answer"}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	var deltas []string
	reply := p.HandleEvents(context.Background(), request(), "tell me something", pipeline.Events{
		Delta: func(delta string) { deltas = append(deltas, delta) },
	})
	if reply.Text != "partial answer" {
		t.Fatalf("reply = %q", reply.Text)
	}
	if len(deltas) != 1 || deltas[0] != "partial" {
		t.Fatalf("deltas = %v, want the hook forwarded", deltas)
	}
}

func TestPendingConfirmationIsReturnedToTheCaller(t *testing.T) {
	runner := newFakeRunner()
	pending := &agent.PendingConfirm{Tool: "mail.send"}
	runner.result = agent.Result{Text: agent.ConfirmPrompt, Pending: pending}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	reply := p.Handle(context.Background(), request(), "email my doctor")
	if reply.Pending == nil || reply.Pending.Tool != "mail.send" {
		t.Fatalf("pending = %+v, want mail.send", reply.Pending)
	}
}

func TestLocalOnlyFastPathTurnIsMarkedInHistory(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	local := tool.Tool[fakeIn, fakeOut]{
		Name:        "mail.inbox",
		Description: "Local only.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler:     func(context.Context, fakeIn) (fakeOut, error) { return fakeOut{Value: "private"}, nil },
		Speak:       func(out fakeOut) string { return out.Value },
	}
	if err := tool.Add(reg, local); err != nil {
		t.Fatalf("Add: %v", err)
	}
	runner := newFakeRunner()
	p := newAgentPipeline(t, runner, reg, map[string]string{"read my inbox": "mail.inbox"})

	p.Handle(context.Background(), request(), "read my inbox")

	turns := runner.History().Recent()
	if len(turns) != 1 || len(turns[0].Calls) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	if !turns[0].Calls[0].LocalOnly {
		t.Fatal("history did not mark the Local-only call")
	}
}

// TestAgentFailureIsVisibleOnTheRequestLine keeps a spoken Unavailable message
// from looking like a successful Agent reply in the log (#27).
func TestAgentFailureIsVisibleOnTheRequestLine(t *testing.T) {
	for _, test := range []struct {
		name   string
		result agent.Result
		want   string
	}{
		{name: "unavailable", result: agent.Result{Text: "Sorry.", Unavailable: true}, want: "unavailable"},
		{name: "timeout", result: agent.Result{Text: "Sorry.", TimedOut: true}, want: "timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newFakeRunner()
			runner.result = test.result
			p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

			timeline := latency.New("req-agent", latency.SourceVoice)
			ctx := latency.WithContext(context.Background(), timeline)
			p.Handle(ctx, tool.Request{ID: "req-agent", Device: tool.DevicePi}, "ask the agent anything")

			var rendered bytes.Buffer
			timeline.Complete(slog.New(slog.NewJSONHandler(&rendered, nil)), latency.OutcomeOK, nil)
			var line map[string]any
			if err := json.Unmarshal(rendered.Bytes(), &line); err != nil {
				t.Fatalf("request line is not JSON: %v", err)
			}
			if line["agent_error"] != test.want {
				t.Fatalf("agent_error = %v, want %q in %v", line["agent_error"], test.want, line)
			}
			if !timeline.Failed() {
				t.Fatal("a spoken Unavailable message left the request marked successful")
			}
		})
	}
}

func TestCancelledAgentTurnIsNotMarkedFailed(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Cancelled: true}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	timeline := latency.New("req-cancelled", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)
	reply := p.Handle(ctx, request(), "tell me a long story")

	if reply.Text != "" {
		t.Fatalf("reply = %q, want no spoken text on cancellation", reply.Text)
	}
	if timeline.Failed() {
		t.Fatal("cancelled agent turn was marked failed")
	}
}

// TestAgentUnavailableMarksTheReply is the seam the reply session uses to play
// the error Cue before the Unavailable message.
func TestAgentUnavailableMarksTheReply(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Sorry, I can't answer that right now.", Unavailable: true}
	p := newAgentPipeline(t, runner, tool.NewRegistry(time.Second), nil)

	reply := p.Handle(context.Background(), tool.Request{ID: "req-agent", Device: tool.DevicePi}, "ask the agent anything")
	if !reply.Unavailable {
		t.Fatalf("reply.Unavailable = false for %+v", reply)
	}
	if reply.Path != tool.PathAgent || reply.Text != runner.result.Text {
		t.Fatalf("reply = %+v", reply)
	}
}
