package pipeline_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// callCounter counts executions so a test can prove a failing-closed route never
// reached a Tool's handler.
type callCounter struct {
	mu sync.Mutex
	n  int
}

func (c *callCounter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *callCounter) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// gatedEmbedder returns vectors from a fixed table. gateNext holds the next
// Embed call open, so a test can observe the window before a rebuild swaps.
type gatedEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	gate    chan struct{}
	calls   int
}

func (e *gatedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	gate := e.gate
	e.gate = nil
	e.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v, ok := e.vectors[text]
		if !ok {
			return nil, errors.New("gated embedder has no vector for: " + text)
		}
		out[i] = v
	}
	return out, nil
}

// gateNext holds the next Embed call open until the returned channel closes.
func (e *gatedEmbedder) gateNext() chan struct{} {
	gate := make(chan struct{})
	e.mu.Lock()
	e.gate = gate
	e.mu.Unlock()
	return gate
}

func (e *gatedEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *gatedEmbedder) waitCalls(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e.callCount() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("embed calls = %d, want %d", e.callCount(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// livePolicy is the production wiring for a live Tool policy change: the real
// settings service, the real registry policy, the real wording worker and the
// real semantic router. Only the Agent model call is faked.
type livePolicy struct {
	pipeline *pipeline.Pipeline
	registry *tool.Registry
	policy   *toolpolicy.Policy
	router   *pipeline.SemanticRouter
	manager  *wording.Manager
	embedder *gatedEmbedder
	runner   *fakeRunner
	alpha    *callCounter
	beta     *callCounter
}

func boolPtr(value bool) *bool { return &value }

// newLivePolicyPipeline registers two semantically routable Tools. The word
// "alpha example" is the best match for both, so removing test.alpha from the
// index leaves test.beta above the threshold with its own margin.
func newLivePolicyPipeline(t *testing.T) *livePolicy {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	reg := tool.NewRegistry(2 * time.Second)
	alpha, beta := &callCounter{}, &callCounter{}
	add := func(name, example, reply string, counter *callCounter) {
		t.Helper()
		if err := tool.Add(reg, tool.Tool[fakeIn, fakeOut]{
			Name:        name,
			Description: "Live policy test tool.",
			Permission:  tool.PermissionRead,
			LocalOnly:   tool.LocalOnlyNo,
			Examples:    []string{example},
			Handler: func(context.Context, fakeIn) (fakeOut, error) {
				counter.inc()
				return fakeOut{Value: reply}, nil
			},
			Speak: func(out fakeOut) string { return out.Value },
		}); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	add("test.alpha", "alpha example", "alpha", alpha)
	add("test.beta", "beta example", "beta", beta)

	cfg := wording.Config{
		RequestPrefixes:    []string{"ask the agent"},
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	}
	setup, err := wording.Build(reg, cfg)
	if err != nil {
		t.Fatalf("wording.Build: %v", err)
	}
	policyDefs, err := toolpolicy.Definitions(reg, toolpolicy.Config{})
	if err != nil {
		t.Fatalf("toolpolicy.Definitions: %v", err)
	}
	defs := append(append([]settings.Definition(nil), setup.Definitions...), policyDefs...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	policy, err := toolpolicy.New(svc, reg)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}
	mgr, err := wording.NewManager(svc, setup, nil, nil, logger,
		wording.WithRetryBackoff(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("wording.NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	embedder := &gatedEmbedder{vectors: map[string][]float32{
		"alpha example": {1, 0},
		"beta example":  {0.8, 0.6},
	}}
	router := pipeline.NewSemanticRouter(reg, embedder, pipeline.SemanticConfig{
		Threshold: 0.7,
		Margin:    0.08,
		Timeout:   time.Second,
	})
	router.SetExamplesSource(mgr)
	examples := mgr.Examples()
	set, err := router.BuildSet(ctx, examples)
	if err != nil {
		t.Fatalf("BuildSet: %v", err)
	}
	router.Swap(set)
	mgr.MarkExamplesBuilt(examples)
	mgr.Start(ctx, router)

	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(runner, cfg.RequestPrefixes),
		pipeline.WithWording(mgr),
		pipeline.WithSemanticRouter(router),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return &livePolicy{
		pipeline: p, registry: reg, policy: policy, router: router,
		manager: mgr, embedder: embedder, runner: runner, alpha: alpha, beta: beta,
	}
}

// TestDisabledToolRoutesToTheNextBestAfterTheRebuild is the issue's pipeline
// regression: before the fix the disabled Tool stayed in the index, resolveArgs
// refused it and the request fell to the Agent path with extract_failed even
// though test.beta would have matched.
func TestDisabledToolRoutesToTheNextBestAfterTheRebuild(t *testing.T) {
	f := newLivePolicyPipeline(t)
	ctx := context.Background()

	if got := f.router.ToolCount(); got != 2 {
		t.Fatalf("initial ToolCount = %d, want 2", got)
	}
	reply := f.pipeline.Handle(ctx, request(), "alpha example")
	if reply.Path != tool.PathFast || reply.Tool != "test.alpha" {
		t.Fatalf("initial path/tool = %q/%q, want the Fast path to test.alpha", reply.Path, reply.Tool)
	}

	if _, err := f.policy.Set(ctx, "test.alpha", toolpolicy.Update{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := f.router.ToolCount(); got != 1 {
		t.Fatalf("after disable ToolCount = %d, want 1", got)
	}

	reply = f.pipeline.Handle(ctx, request(), "alpha example")
	if reply.Path != tool.PathFast || reply.Tool != "test.beta" {
		t.Fatalf("after disable path/tool = %q/%q, want the Fast path to test.beta", reply.Path, reply.Tool)
	}
	if reply.Text != "beta" {
		t.Fatalf("after disable text = %q, want beta", reply.Text)
	}
	if got := f.alpha.calls(); got != 1 {
		t.Fatalf("disabled Tool ran %d times, want only its pre-disable call", got)
	}
	if got := f.runner.calls(); len(got) != 0 {
		t.Fatalf("agent saw %v, want the Fast path to handle it", got)
	}
}

// TestDisabledToolInFlightIsNeitherPromptedNorRoutable disables a Tool while
// its old vectors are still live (the rebuild is held in the embedder). Before
// the swap the index still scores it, so resolveArgs must stay the last line of
// defence: the Tool never runs, the prompt no longer lists it, and the next
// turn after Wait routes to the next best Tool.
func TestDisabledToolInFlightIsNeitherPromptedNorRoutable(t *testing.T) {
	f := newLivePolicyPipeline(t)
	ctx := context.Background()

	gate := f.embedder.gateNext()
	before := f.embedder.callCount()
	if _, err := f.policy.Set(ctx, "test.alpha", toolpolicy.Update{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// The rebuild is blocked in the embedder: the old vectors are still live.
	f.embedder.waitCalls(t, before+1)
	if got := f.router.ToolCount(); got != 2 {
		t.Fatalf("in-flight ToolCount = %d, want the old 2", got)
	}

	reply := f.pipeline.Handle(ctx, request(), "alpha example")
	if reply.Path != tool.PathAgent {
		t.Fatalf("in-flight path = %q, want the Agent path (fail closed)", reply.Path)
	}
	if got := f.alpha.calls(); got != 0 {
		t.Fatalf("disabled Tool ran %d times before the swap, want 0", got)
	}
	// The one prompt builder already omits it, so the model cannot call it.
	for _, offered := range agent.NewPromptBuilder("").Tools(f.registry) {
		if offered.OfFunction != nil && offered.OfFunction.Function.Name == "test_alpha" {
			t.Fatal("disabled Tool is still offered to the model")
		}
	}

	close(gate)
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	reply = f.pipeline.Handle(ctx, request(), "alpha example")
	if reply.Path != tool.PathFast || reply.Tool != "test.beta" {
		t.Fatalf("after swap path/tool = %q/%q, want the Fast path to test.beta", reply.Path, reply.Tool)
	}
	if got := f.alpha.calls(); got != 0 {
		t.Fatalf("disabled Tool ran %d times, want 0", got)
	}
}

// fixedWording is a live phrase table without the settings service: the shape a
// caller can supply after construction.
type fixedWording struct {
	words pipeline.RoutingWords
}

func (f fixedWording) Routing() pipeline.RoutingWords { return f.words }

// TestLiveControlPhraseCannotTargetAnAgentOnlyTool enforces the invariant for
// any caller-supplied phrase table: pipeline.New rejects an AgentOnly target,
// but a live table must also fall through instead of executing it.
func TestLiveControlPhraseCannotTargetAnAgentOnlyTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	executed := &callCounter{}
	if err := tool.Add(reg, tool.Tool[fakeIn, fakeOut]{
		Name:        "web.search",
		Description: "Agent-only test tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   true,
		Handler: func(context.Context, fakeIn) (fakeOut, error) {
			executed.inc()
			return fakeOut{Value: "search result"}, nil
		},
		Speak: func(out fakeOut) string { return out.Value },
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithWording(fixedWording{words: pipeline.RoutingWords{
			ControlPhrases: map[string]string{"search the web": "web.search"},
		}}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "search the web")
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the Agent path", reply.Path)
	}
	if got := executed.calls(); got != 0 {
		t.Fatalf("AgentOnly Tool executed %d times from a Control phrase, want 0", got)
	}
	if reply.Tool == "web.search" {
		t.Fatal("reply names the AgentOnly Tool as a Fast-path call")
	}
}
