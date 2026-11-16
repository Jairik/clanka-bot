package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// staticPolicy is a minimal live Tool policy: it can disable a Tool or raise
// its Permission without the settings service. Unlisted Tools keep their code
// declarations.
type staticPolicy struct {
	settings map[string]tool.ToolSettings
}

func (s staticPolicy) Effective(name string, declared tool.Permission) tool.ToolSettings {
	if v, ok := s.settings[name]; ok {
		return v
	}
	return tool.ToolSettings{Enabled: true, Permission: declared}
}

// livePhrasePipeline builds a pipeline whose Control phrase table is supplied
// by a caller after construction, the shape the single-phrase AgentOnly guard
// already treats as real.
func livePhrasePipeline(t *testing.T, reg *tool.Registry, phrases map[string]string, runner *fakeRunner) *pipeline.Pipeline {
	t.Helper()
	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithWording(fixedWording{words: pipeline.RoutingWords{ControlPhrases: phrases}}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// addCountingTool registers a normal, Fast-path-eligible Tool whose handler is
// counted, so a test can prove a handler did or did not run.
func addCountingTool(t *testing.T, reg *tool.Registry, name string, counter *callCounter) {
	t.Helper()
	tll := fakeTool(name, func(context.Context, fakeIn) (fakeOut, error) {
		counter.inc()
		return fakeOut{Value: name}, nil
	})
	if err := tool.Add(reg, tll); err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

// addCountingAgentOnlyTool registers an AgentOnly Tool with a Speak renderer
// (the shape New rejects in a code phrase table) whose handler is counted.
func addCountingAgentOnlyTool(t *testing.T, reg *tool.Registry, name string, counter *callCounter) {
	t.Helper()
	tll := fakeTool(name, func(context.Context, fakeIn) (fakeOut, error) {
		counter.inc()
		return fakeOut{Value: name}, nil
	})
	tll.AgentOnly = true
	if err := tool.Add(reg, tll); err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

// The chain planner runs before the single-phrase AgentOnly guard. A
// caller-supplied table that names an AgentOnly Tool in a two-step chain must
// not execute either Tool: the whole utterance goes to the Agent path.
func TestChainCannotExecuteAgentOnlyToolFromLivePhraseTable(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	agentOnlyCalls, pauseCalls := &callCounter{}, &callCounter{}
	addCountingAgentOnlyTool(t, reg, "web.search", agentOnlyCalls)
	addCountingTool(t, reg, "test.pause", pauseCalls)
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p := livePhrasePipeline(t, reg, map[string]string{
		"search the web":  "web.search",
		"pause the music": "test.pause",
	}, runner)
	p.BindChains(func() bool { return true })

	reply := p.Handle(context.Background(), request(), "search the web and pause the music")
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the Agent path (reply %q)", reply.Path, reply.Text)
	}
	if got := agentOnlyCalls.calls(); got != 0 {
		t.Errorf("AgentOnly Tool handler ran %d times, want 0", got)
	}
	if got := pauseCalls.calls(); got != 0 {
		t.Errorf("second chain Tool handler ran %d times, want 0", got)
	}
	if got := len(runner.calls()); got != 1 {
		t.Fatalf("Agent calls = %d, want 1", got)
	}
}

// A confirm Tool named by a live table is just as ineligible for a chain. The
// handler must never run and no one-turn Confirmation is armed.
func TestChainCannotExecuteConfirmToolFromLivePhraseTable(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	confirmCalls, pauseCalls := &callCounter{}, &callCounter{}
	confirmTool := fakeTool("test.confirm", func(context.Context, fakeIn) (fakeOut, error) {
		confirmCalls.inc()
		return fakeOut{Value: "confirmed"}, nil
	})
	confirmTool.Permission = tool.PermissionConfirm
	if err := tool.Add(reg, confirmTool); err != nil {
		t.Fatalf("Add test.confirm: %v", err)
	}
	addCountingTool(t, reg, "test.pause", pauseCalls)
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p := livePhrasePipeline(t, reg, map[string]string{
		"confirm the thing": "test.confirm",
		"pause the music":   "test.pause",
	}, runner)
	p.BindChains(func() bool { return true })

	reply := p.Handle(context.Background(), request(), "confirm the thing and pause the music")
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the Agent path (reply %q)", reply.Path, reply.Text)
	}
	if got := confirmCalls.calls(); got != 0 {
		t.Errorf("confirm Tool handler ran %d times, want 0", got)
	}
	if got := pauseCalls.calls(); got != 0 {
		t.Errorf("second chain Tool handler ran %d times, want 0", got)
	}
	if reply.Pending != nil {
		t.Error("chain armed a Confirmation for an ineligible Tool")
	}
}

// deferredRequest used to accept any non-confirm name. An AgentOnly Tool in a
// live table must not be queued; the utterance takes the Agent path instead.
func TestDeferredCannotQueueAgentOnlyTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	agentOnlyCalls := &callCounter{}
	addCountingAgentOnlyTool(t, reg, "web.search", agentOnlyCalls)
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p := livePhrasePipeline(t, reg, map[string]string{
		"search the web": "web.search",
	}, runner)
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)

	reply := p.Handle(context.Background(), request(), "after the timer search the web")
	if len(q.List()) != 0 {
		t.Errorf("AgentOnly action was queued: %+v", q.List())
	}
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the Agent path (reply %q)", reply.Path, reply.Text)
	}
	if got := agentOnlyCalls.calls(); got != 0 {
		t.Errorf("AgentOnly Tool handler ran %d times, want 0", got)
	}
	if got := len(runner.calls()); got != 1 {
		t.Fatalf("Agent calls = %d, want 1", got)
	}
}

// A Tool disabled by the live policy is not routable either, so a deferred
// action naming it falls through to the Agent path rather than being queued.
func TestDeferredCannotQueueDisabledTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	pauseCalls := &callCounter{}
	addCountingTool(t, reg, "test.pause", pauseCalls)
	reg.SetPolicy(staticPolicy{settings: map[string]tool.ToolSettings{
		"test.pause": {Enabled: false, Permission: tool.PermissionRead},
	}})
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p := livePhrasePipeline(t, reg, map[string]string{
		"pause the music": "test.pause",
	}, runner)
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)

	reply := p.Handle(context.Background(), request(), "after the timer pause the music")
	if len(q.List()) != 0 {
		t.Errorf("disabled action was queued: %+v", q.List())
	}
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the Agent path (reply %q)", reply.Path, reply.Text)
	}
	if got := pauseCalls.calls(); got != 0 {
		t.Errorf("disabled Tool handler ran %d times, want 0", got)
	}
}

// Normal Fast-path Tools still run through both fixed sites: a chain executes
// both leaves, and a deferred request queues a safe leaf.
func TestNormalFastPathToolsStillRun(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	pauseCalls, resumeCalls := &callCounter{}, &callCounter{}
	addCountingTool(t, reg, "test.pause", pauseCalls)
	addCountingTool(t, reg, "test.resume", resumeCalls)
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p := livePhrasePipeline(t, reg, map[string]string{
		"pause the music":  "test.pause",
		"resume the music": "test.resume",
	}, runner)
	p.BindChains(func() bool { return true })
	q := &pipeline.DeferredQueue{}
	p.BindDeferred(q)

	reply := p.Handle(context.Background(), request(), "pause the music and resume the music")
	if reply.Path != tool.PathFast || reply.Tool != "test.resume" {
		t.Fatalf("chain path/tool = %q/%q, want fast path/test.resume", reply.Path, reply.Tool)
	}
	if got := pauseCalls.calls(); got != 1 {
		t.Errorf("first chain Tool ran %d times, want 1", got)
	}
	if got := resumeCalls.calls(); got != 1 {
		t.Errorf("second chain Tool ran %d times, want 1", got)
	}

	reply = p.Handle(context.Background(), request(), "after the timer pause the music")
	if reply.Path != tool.PathControlPhrase {
		t.Fatalf("deferred path = %q, want Control phrase", reply.Path)
	}
	queued := q.List()
	if len(queued) != 1 || queued[0].Text != "pause the music" {
		t.Fatalf("deferred queue = %+v, want one safe leaf", queued)
	}
	if got := len(runner.calls()); got != 0 {
		t.Fatalf("Agent saw %v, want no fallback for safe Tools", runner.calls())
	}
}
