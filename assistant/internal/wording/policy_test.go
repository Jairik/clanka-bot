package wording_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// newPolicyFixture wires the live Tool policy behind the real settings service,
// so a disabled or tightened Tool reaches the router the way it does in main.
func newPolicyFixture(t *testing.T, opts ...wording.Option) (*fixture, *toolpolicy.Policy) {
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
	reg := testRegistry(t)
	// wording.Build must run before the policy is installed, while registry.Info
	// still returns the code declarations.
	setup, err := wording.Build(reg, testConfig())
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
	opts = append([]wording.Option{wording.WithRetryBackoff(time.Hour, time.Hour)}, opts...)
	mgr, err := wording.NewManager(svc, setup, nil, nil, logger, opts...)
	if err != nil {
		t.Fatalf("wording.NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)
	policy, err := toolpolicy.New(svc, reg)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}
	return &fixture{settings: svc, manager: mgr, registry: reg, setup: setup}, policy
}

// startSemantic builds and starts the real manager as the router's Examples
// source, which is how main wires the live index.
func startSemantic(t *testing.T, f *fixture) (*pipeline.SemanticRouter, *fakeEmbedder) {
	t.Helper()
	embedder := &fakeEmbedder{}
	router := pipeline.NewSemanticRouter(f.registry, embedder, pipeline.SemanticConfig{
		Threshold: 0.7,
		Margin:    0,
		Timeout:   time.Second,
	})
	router.SetExamplesSource(f.manager)
	examples := f.manager.Examples()
	set, err := router.BuildSet(context.Background(), examples)
	if err != nil {
		t.Fatalf("BuildSet: %v", err)
	}
	router.Swap(set)
	f.manager.MarkExamplesBuilt(examples)
	f.manager.Start(context.Background(), router)
	return router, embedder
}

func boolPtr(value bool) *bool { return &value }

// TestDisablingAToolRemovesItFromTheLiveIndex is the live-policy regression:
// a Dashboard toggle changes tools.<name>.enabled but not the Examples text, so
// the rebuild must be driven by the eligibility change alone.
func TestDisablingAToolRemovesItFromTheLiveIndex(t *testing.T) {
	f, policy := newPolicyFixture(t)
	router, _ := startSemantic(t, f)
	ctx := context.Background()

	if got := router.ToolCount(); got != 2 {
		t.Fatalf("initial ToolCount = %d, want 2", got)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("initial match = %q, want test.clock", got.Tool)
	}

	if _, err := policy.Set(ctx, "test.clock", toolpolicy.Update{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.ToolCount(); got != 1 {
		t.Fatalf("after disable ToolCount = %d, want 1", got)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("disabled Tool still top-1: %q", got.Tool)
	}
	for _, score := range router.Match(ctx, "what time is it").Top {
		if score.Tool == "test.clock" {
			t.Fatal("disabled Tool is still scored")
		}
	}
}

// TestTighteningAToolToConfirmRemovesItFromTheLiveIndex covers the other
// eligibility change Changed wakes for: raising read to confirm leaves the
// Examples untouched but must drop the Tool from the semantic index.
func TestTighteningAToolToConfirmRemovesItFromTheLiveIndex(t *testing.T) {
	f, policy := newPolicyFixture(t)
	router, embedder := startSemantic(t, f)
	ctx := context.Background()

	confirm := tool.PermissionConfirm
	if _, err := policy.Set(ctx, "test.clock", toolpolicy.Update{Permission: &confirm}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.ToolCount(); got != 1 {
		t.Fatalf("after tighten ToolCount = %d, want 1", got)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("confirm-raised Tool still top-1: %q", got.Tool)
	}

	// Removing the override restores the declared read Permission and re-adds
	// the Tool with one rebuild.
	gate := embedder.gateNext()
	before := embedder.callCount()
	if _, err := policy.Reset(ctx, "test.clock"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	embedder.waitCalls(t, before+1)
	close(gate)
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.ToolCount(); got != 2 {
		t.Fatalf("after reset ToolCount = %d, want 2", got)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("reset Tool match = %q, want test.clock", got.Tool)
	}
}

// TestEligibilityRebuildRetriesAfterAFailedEmbed proves the pending-rebuild
// flag survives a failed build: the bounded retry keeps trying, so disabling a
// Tool during an embed outage still reaches the index once the service returns.
func TestEligibilityRebuildRetriesAfterAFailedEmbed(t *testing.T) {
	f, policy := newPolicyFixture(t, shortRetry())
	router, embedder := startSemantic(t, f)
	ctx := context.Background()

	embedder.mu.Lock()
	embedder.failErr = errors.New("embed service is down")
	embedder.mu.Unlock()
	if _, err := policy.Set(ctx, "test.clock", toolpolicy.Update{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := router.ToolCount(); got != 2 {
		t.Fatalf("failed rebuild ToolCount = %d, want the old 2", got)
	}

	// Let at least two retries fail, then clear the outage: recovery must not
	// depend on a single tick.
	failed := embedder.callCount()
	waitFor(t, "a failed eligibility retry", func() bool { return embedder.callCount() >= failed+2 })
	embedder.mu.Lock()
	embedder.failErr = nil
	embedder.mu.Unlock()
	waitFor(t, "the eligibility rebuild", func() bool { return router.ToolCount() == 1 })
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("disabled Tool still top-1 after recovery: %q", got.Tool)
	}
}

// TestBuildDefinesNoControlPhrasesForAnAgentOnlyTool proves the latent hole is
// closed at the definition layer: an AgentOnly Tool with a Speak renderer gets
// no live Control phrase list, so the Dashboard never offers an editor for a
// Fast-path route the Tool must never have.
func TestBuildDefinesNoControlPhrasesForAnAgentOnlyTool(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[testIn, testOut]{
		Name:        "web.search",
		Description: "Agent-only test tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   true,
		Handler: func(context.Context, testIn) (testOut, error) {
			return testOut{Text: "result"}, nil
		},
		Speak: func(out testOut) string { return out.Text },
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := testConfig()
	// The only registered Tool is the AgentOnly one, so no config phrase can
	// name a valid Control-phrase target.
	cfg.ControlPhrases = nil
	setup, err := wording.Build(reg, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := definitionsByKey(setup.Definitions)[wording.ControlPhrasesKey("web.search")]; ok {
		t.Fatal("Build defined a Control phrase list for an AgentOnly Tool")
	}
	for _, name := range setup.ControlTools {
		if name == "web.search" {
			t.Fatal("Setup.ControlTools lists an AgentOnly Tool")
		}
	}
}

// TestRoutingIgnoresAStaleAgentOnlyControlPhraseRow simulates the migrated
// database the fix cannot clean: the row exists, but the manager never reads it
// because Setup.ControlTools excludes the AgentOnly Tool.
func TestRoutingIgnoresAStaleAgentOnlyControlPhraseRow(t *testing.T) {
	ctx := context.Background()
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[testIn, testOut]{
		Name:        "web.search",
		Description: "Agent-only test tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   true,
		Handler: func(context.Context, testIn) (testOut, error) {
			return testOut{Text: "result"}, nil
		},
		Speak: func(out testOut) string { return out.Text },
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := testConfig()
	cfg.ControlPhrases = nil
	setup, err := wording.Build(reg, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// A row from before the fix, kept so the test proves Routing drops it.
	stale := settings.Strings(wording.ControlPhrasesKey("web.search"), []string{"search the web"})
	defs := append(append([]settings.Definition(nil), setup.Definitions...), stale)
	svc, err := settings.New(ctx, openStore(t), defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if value, ok := svc.Get(wording.ControlPhrasesKey("web.search")); !ok {
		t.Fatalf("stale row missing: %+v", value)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := wording.NewManager(svc, setup, nil, nil, logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)
	mgr.Start(ctx, nil)

	if owner, ok := mgr.Routing().ControlPhrases["search the web"]; ok {
		t.Fatalf("stale AgentOnly Control phrase routed to %q", owner)
	}
}
