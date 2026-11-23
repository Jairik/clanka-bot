package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// cutoverClockPipeline is a Fast-path clock plus optional Agent and Hermes
// seams, the smallest surface the #526 acceptance table needs.
func cutoverClockPipeline(t *testing.T, runner pipeline.AgentRunner, hermes func(context.Context, string) string) *pipeline.Pipeline {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	logger, _ := testLogger()
	opts := []pipeline.Option{
		pipeline.WithAgent(runner, []string{"ask the agent", "agent", "hey agent"}),
	}
	if hermes != nil {
		opts = append(opts, pipeline.WithHermes(hermes))
	}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// TestCutoverEntryPointsShareAgentFallback is the pre-cutover contract for
// #526: voice and typed Dashboard turns use the same pipeline, so a miss and
// an explicit Agent request both reach the Go loop today. After Hermes is the
// fallback these still share one seam; only the runner behind PathAgent
// changes.
func TestCutoverEntryPointsShareAgentFallback(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Here is an answer."}
	p := cutoverClockPipeline(t, runner, nil)

	sources := []latency.Source{latency.SourceVoice, latency.SourceDashboard, latency.SourceDashboardMic}
	for i, source := range sources {
		timeline := latency.New("req-cutover-"+string(source), source)
		ctx := latency.WithContext(context.Background(), timeline)
		reply := p.Handle(ctx, tool.Request{ID: timeline.ID(), Device: tool.DevicePi}, "why is the sky blue")
		if reply.Path != tool.PathAgent {
			t.Fatalf("%s path = %q, want the Agent fallback", source, reply.Path)
		}
		if reply.Text != "Here is an answer." {
			t.Fatalf("%s reply = %q", source, reply.Text)
		}
		if got := runner.calls(); len(got) != i+1 || got[i] != "why is the sky blue" {
			t.Fatalf("%s agent saw %v", source, got)
		}
	}
}

// TestCutoverPrefixesStayOnTheirOwnPath pins today's two model prefixes so a
// later swap of the Agent fallback to Hermes cannot mix them by accident:
// "ask the agent" is the Go loop, "ask hermes" is the explicit job, and a
// Control phrase never reaches either.
func TestCutoverPrefixesStayOnTheirOwnPath(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	var hermesTasks []string
	p := cutoverClockPipeline(t, runner, func(_ context.Context, task string) string {
		hermesTasks = append(hermesTasks, task)
		return "Hermes is working on job 1."
	})

	agentReply := p.Handle(context.Background(), request(), "ask the agent why is the sky blue")
	if agentReply.Path != tool.PathAgent || agentReply.Text != "Agent answer." {
		t.Fatalf("agent prefix reply = %+v", agentReply)
	}
	if calls := runner.calls(); len(calls) != 1 || calls[0] != "why is the sky blue" {
		t.Fatalf("agent saw %v, want the prefix stripped", calls)
	}

	hermesReply := p.Handle(context.Background(), request(), "Ask Hermes: Explain HTTP")
	if hermesReply.Path != tool.PathHermes || !hermesReply.LocalOnly {
		t.Fatalf("hermes reply = %+v, want a Local-only Hermes path", hermesReply)
	}
	if hermesReply.Text != "Hermes is working on job 1." {
		t.Fatalf("hermes text = %q", hermesReply.Text)
	}
	if len(hermesTasks) != 1 || hermesTasks[0] != "Explain HTTP" {
		t.Fatalf("hermes saw %v", hermesTasks)
	}
	if calls := runner.calls(); len(calls) != 1 {
		t.Fatalf("explicit Hermes input reached the Agent path: %v", calls)
	}

	fast := p.Handle(context.Background(), request(), "what time is it")
	if fast.Path != tool.PathControlPhrase || fast.Tool != clock.Name {
		t.Fatalf("control phrase = %+v, want the Fast path", fast)
	}
	if calls := runner.calls(); len(calls) != 1 {
		t.Fatalf("control phrase reached the Agent path: %v", calls)
	}
}

// TestCutoverDisabledHermesOwnsItsPrefixAndLeavesFastPathUp is the fail-closed
// gate: Hermes down still claims "ask hermes" so it cannot leak into the Go
// loop, and a Control phrase still runs.
func TestCutoverDisabledHermesOwnsItsPrefixAndLeavesFastPathUp(t *testing.T) {
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "should not run for Ask Hermes"}
	p := cutoverClockPipeline(t, runner, nil)

	disabled := p.Handle(context.Background(), request(), "ask hermes what is two plus two")
	if disabled.Path != tool.PathHermes || disabled.Text != "Hermes is disabled." {
		t.Fatalf("disabled hermes = %+v", disabled)
	}
	if calls := runner.calls(); len(calls) != 0 {
		t.Fatalf("disabled Hermes fell through to the Agent path: %v", calls)
	}

	fast := p.Handle(context.Background(), request(), "what time is it")
	if fast.Path != tool.PathControlPhrase || fast.Text != "It's 3:04 PM." {
		t.Fatalf("fast path with Hermes disabled = %+v", fast)
	}
}

// TestCutoverMissingAgentRunnerLeavesGoCommandsWorking is the current stand-in
// for "unavailable Hermes, Go commands still work": with no Agent runner, a
// Control phrase still executes and an unmatched request stays off any model.
func TestCutoverMissingAgentRunnerLeavesGoCommandsWorking(t *testing.T) {
	logger, _ := testLogger()
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	fast := p.Handle(context.Background(), request(), "what time is it")
	if fast.Path != tool.PathControlPhrase || fast.Text != "It's 3:04 PM." {
		t.Fatalf("control phrase without an Agent runner = %+v", fast)
	}

	miss := p.Handle(context.Background(), request(), "why is the sky blue")
	if miss.Path != tool.PathFallthrough || miss.Text != pipeline.PlaceholderReply {
		t.Fatalf("unmatched without an Agent runner = %+v, want the placeholder", miss)
	}
}

// TestCutoverConfirmationSettlesBeforeEitherModelPath keeps Confirmation
// Go-owned: a pending yes never reaches Hermes or the Agent loop, which is
// the rule #525 must reuse for outward writes.
func TestCutoverConfirmationSettlesBeforeEitherModelPath(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)
	runner := newConfirmingRunner(reg)
	var hermesTasks []string
	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(runner, []string{"ask the agent"}),
		pipeline.WithHermes(func(_ context.Context, task string) string {
			hermesTasks = append(hermesTasks, task)
			return "Hermes should not see a yes."
		}),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	askForConfirmation(t, p, runner, &runs)
	reply := p.Handle(context.Background(), request(), "yes")
	if runs != 1 {
		t.Fatalf("confirm handler ran %d times, want 1", runs)
	}
	if reply.Path != tool.PathConfirmation || reply.Tool != "mail.send" {
		t.Fatalf("yes = %+v, want the Go Confirmation path", reply)
	}
	if calls := runner.seen(); len(calls) != 1 {
		t.Fatalf("yes reached the Agent path: %v", calls)
	}
	if len(hermesTasks) != 0 {
		t.Fatalf("yes reached Hermes: %v", hermesTasks)
	}
}
