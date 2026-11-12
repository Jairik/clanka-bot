package wording_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// shortRetry keeps the bounded retry observable without slowing the suite.
func shortRetry() wording.Option {
	return wording.WithRetryBackoff(5*time.Millisecond, 20*time.Millisecond)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestExamplesBuildFailureRecoversInTheBackground is the startup regression:
// the router is attached even when the first build fails, and a later success
// swaps in without a config edit or restart.
func TestExamplesBuildFailureRecoversInTheBackground(t *testing.T) {
	f := newFixtureWith(t, nil, nil, nil, shortRetry())
	embedder := &fakeEmbedder{}
	embedder.mu.Lock()
	embedder.failErr = errors.New("embed service is down")
	embedder.mu.Unlock()

	router := pipeline.NewSemanticRouter(f.registry, embedder, pipeline.SemanticConfig{
		Threshold: 0.7,
		Margin:    0,
		Timeout:   time.Second,
	})
	router.SetExamplesSource(f.manager)
	ctx := context.Background()
	if _, err := router.BuildSet(ctx, f.manager.Examples()); err == nil {
		t.Fatal("BuildSet succeeded while the embedder was failing")
	}
	if got := router.ToolCount(); got != 0 {
		t.Fatalf("failed startup build indexed %d Tools", got)
	}

	f.manager.Start(ctx, router)
	// The worker retries on its own bounded schedule. Let at least two retries
	// fail, then clear the failure: recovery must not depend on a single tick.
	waitFor(t, "two failed background retries", func() bool { return embedder.callCount() >= 4 })
	embedder.mu.Lock()
	embedder.failErr = nil
	embedder.mu.Unlock()
	waitFor(t, "the background Examples rebuild", func() bool { return router.ToolCount() > 0 })
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("recovered index routed to %q, want test.clock", got.Tool)
	}
}

// TestExamplesRebuildDoesNotSpinWhenUnchanged proves the retry is tied to a
// failure, not a periodic rebuild of an unchanged snapshot.
func TestExamplesRebuildDoesNotSpinWhenUnchanged(t *testing.T) {
	f := newFixture(t, nil, nil)
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

	before := embedder.callCount()
	if err := f.manager.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := embedder.callCount(); got != before {
		t.Fatalf("embed calls = %d, want the unchanged snapshot not to rebuild (%d)", got, before)
	}
}

// TestBackgroundRetryStopsCleanly proves Close cancels a pending retry and
// joins the worker promptly.
func TestBackgroundRetryStopsCleanly(t *testing.T) {
	f := newFixtureWith(t, nil, nil, nil, shortRetry())
	embedder := &fakeEmbedder{}
	embedder.mu.Lock()
	embedder.failErr = errors.New("embed service is down")
	embedder.mu.Unlock()
	router := pipeline.NewSemanticRouter(f.registry, embedder, pipeline.SemanticConfig{
		Threshold: 0.7,
		Margin:    0,
		Timeout:   time.Second,
	})
	router.SetExamplesSource(f.manager)
	f.manager.Start(context.Background(), router)
	waitFor(t, "the first failed rebuild", func() bool { return embedder.callCount() > 0 })

	done := make(chan struct{})
	go func() {
		f.manager.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not join the retry worker")
	}
	after := embedder.callCount()
	time.Sleep(30 * time.Millisecond)
	if got := embedder.callCount(); got != after {
		t.Fatalf("embed calls continued after Close: %d then %d", after, got)
	}
}

// TestFillerSynthesisFailureRetriesInTheBackground covers S3: a transient TTS
// failure no longer waits for another text or voice change.
func TestFillerSynthesisFailureRetriesInTheBackground(t *testing.T) {
	var calls atomic.Int32
	synth := func(_ context.Context, text string) (*speech.Filler, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("tts is warming up")
		}
		return &speech.Filler{Text: text, PCM: []byte(text)}, nil
	}
	initial := &speech.Filler{Text: "One moment.", PCM: []byte("initial")}
	f := newFixtureWith(t, initial, synth, nil, shortRetry())
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	if _, err := f.settings.Set(ctx, wording.FillerKey, "Two moments."); err != nil {
		t.Fatalf("Set: %v", err)
	}
	waitFor(t, "the filler retry to succeed", func() bool {
		filler := f.manager.Filler()
		return filler != nil && filler.Text == "Two moments."
	})
	if got := f.manager.Filler().Text; got != "Two moments." {
		t.Fatalf("filler = %q after retry", got)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("synthesis calls = %d, want a retry after the first failure", got)
	}
}
