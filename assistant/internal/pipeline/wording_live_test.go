package pipeline_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

type liveWording struct {
	pipeline *pipeline.Pipeline
	runner   *fakeRunner
	settings *settings.Service
	manager  *wording.Manager
	phrases  map[string]string
	prefixes []string
}

// newLiveWordingPipeline wires the real settings service and wording manager
// behind the public pipeline seam. Only the Agent model call is faked.
func newLiveWordingPipeline(t *testing.T) *liveWording {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := tool.NewRegistry(2 * time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("clock.Register: %v", err)
	}

	cfg := wording.Config{
		ControlPhrases:     map[string]string{"what time is it": clock.Name},
		RequestPrefixes:    []string{"ask the agent"},
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	}
	setup, err := wording.Build(reg, cfg)
	if err != nil {
		t.Fatalf("wording.Build: %v", err)
	}
	svc, err := settings.New(ctx, st, setup.Definitions...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	manager, err := wording.NewManager(svc, setup, nil, nil, logger)
	if err != nil {
		t.Fatalf("wording.NewManager: %v", err)
	}
	t.Cleanup(manager.Close)
	manager.Start(ctx, nil)

	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Agent answer."}
	p, err := pipeline.New(reg, cfg.ControlPhrases, logger,
		pipeline.WithAgent(runner, cfg.RequestPrefixes),
		pipeline.WithWording(manager),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return &liveWording{pipeline: p, runner: runner, settings: svc, manager: manager, phrases: cfg.ControlPhrases, prefixes: cfg.RequestPrefixes}
}

func TestControlPhraseOverridesApplyOnTheNextRequest(t *testing.T) {
	f := newLiveWordingPipeline(t)
	ctx := context.Background()

	reply := f.pipeline.Handle(ctx, request(), "what time is it")
	if reply.Path != tool.PathControlPhrase || reply.Tool != clock.Name {
		t.Fatalf("initial path/tool = %q/%q, want the Control phrase", reply.Path, reply.Tool)
	}

	if _, err := f.settings.Set(ctx, wording.ControlPhrasesKey(clock.Name), []string{"current time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The replaced phrase is no longer a Control phrase; this utterance falls
	// through to the Agent path.
	reply = f.pipeline.Handle(ctx, request(), "what time is it")
	if reply.Path != tool.PathAgent {
		t.Fatalf("after override path = %q, want agent", reply.Path)
	}
	// The new phrase applies immediately.
	reply = f.pipeline.Handle(ctx, request(), "current time")
	if reply.Path != tool.PathControlPhrase || reply.Tool != clock.Name {
		t.Fatalf("new phrase path/tool = %q/%q", reply.Path, reply.Tool)
	}

	// Reset restores the config.yaml phrase.
	if _, err := f.settings.Reset(ctx, wording.ControlPhrasesKey(clock.Name)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	reply = f.pipeline.Handle(ctx, request(), "what time is it")
	if reply.Path != tool.PathControlPhrase || reply.Tool != clock.Name {
		t.Fatalf("after reset path/tool = %q/%q", reply.Path, reply.Tool)
	}
}

func TestAgentPrefixOverridesApplyOnTheNextRequest(t *testing.T) {
	f := newLiveWordingPipeline(t)
	ctx := context.Background()

	if _, err := f.settings.Set(ctx, wording.PrefixesKey, []string{"launch"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	reply := f.pipeline.Handle(ctx, request(), "launch do the thing")
	if reply.Path != tool.PathAgent {
		t.Fatalf("launch path = %q, want agent", reply.Path)
	}
	calls := f.runner.calls()
	if len(calls) != 1 || calls[0] != "do the thing" {
		t.Fatalf("agent saw %v, want the new prefix stripped", calls)
	}

	// The old prefix no longer strips, so the full utterance reaches the model.
	reply = f.pipeline.Handle(ctx, request(), "ask the agent do the thing")
	if reply.Path != tool.PathAgent {
		t.Fatalf("old prefix path = %q, want agent", reply.Path)
	}
	calls = f.runner.calls()
	if len(calls) != 2 || calls[1] != "ask the agent do the thing" {
		t.Fatalf("agent saw %v, want the whole utterance after reset", calls)
	}

	if _, err := f.settings.Reset(ctx, wording.PrefixesKey); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	reply = f.pipeline.Handle(ctx, request(), "ask the agent do the thing")
	if reply.Path != tool.PathAgent {
		t.Fatalf("reset prefix path = %q", reply.Path)
	}
	calls = f.runner.calls()
	if len(calls) != 3 || calls[2] != "do the thing" {
		t.Fatalf("agent saw %v, want the config prefix restored", calls)
	}
}
