package wording_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// TestExamplesStateTracksRebuildAndRecovery covers the Health page's view of
// the index: a successful build reports its Tool count, a failed later rebuild
// records the error while keeping the old vectors live, and a recovery clears
// it. All of it is in-memory; no service is probed.
func TestExamplesStateTracksRebuildAndRecovery(t *testing.T) {
	f := newFixture(t, nil, nil)
	embedder := &fakeEmbedder{}
	router := pipeline.NewSemanticRouter(f.registry, embedder, pipeline.SemanticConfig{
		Threshold: 0.7,
		Margin:    0,
		Timeout:   time.Second,
	})
	router.SetExamplesSource(f.manager)
	ctx := context.Background()

	examples := f.manager.Examples()
	set, err := router.BuildSet(ctx, examples)
	if err != nil {
		t.Fatalf("BuildSet: %v", err)
	}
	router.Swap(set)
	f.manager.MarkExamplesBuilt(examples)
	f.manager.Start(ctx, router)

	tools, lastErr, at := f.manager.ExamplesState()
	if tools == 0 {
		t.Fatalf("tool count after a successful build = %d, want > 0", tools)
	}
	if lastErr != nil {
		t.Fatalf("last error after a successful build = %v, want nil", lastErr)
	}
	if at.IsZero() {
		t.Fatal("last attempt is zero after a successful build")
	}
	builtAt := at

	// A later rebuild fails: the old index stays live, but the error and the
	// attempt time are exposed for the Health page.
	embedder.mu.Lock()
	embedder.failErr = errors.New("embed service is down")
	embedder.mu.Unlock()
	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"a new clock phrase"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	tools, lastErr, at = f.manager.ExamplesState()
	if tools == 0 {
		t.Fatalf("failed rebuild dropped the live index: %d tools", tools)
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "embed service is down") {
		t.Fatalf("last error after a failed rebuild = %v", lastErr)
	}
	if !at.After(builtAt) {
		t.Fatalf("attempt time did not advance: %v then %v", builtAt, at)
	}

	// A later successful rebuild clears the error again.
	embedder.mu.Lock()
	embedder.failErr = nil
	embedder.mu.Unlock()
	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"the newest clock phrase"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	tools, lastErr, at = f.manager.ExamplesState()
	if tools == 0 {
		t.Fatalf("tool count after recovery = %d, want > 0", tools)
	}
	if lastErr != nil {
		t.Fatalf("last error after recovery = %v, want nil", lastErr)
	}
	if !at.After(builtAt) {
		t.Fatalf("attempt time did not advance after recovery: %v", at)
	}
}

// TestExamplesStateReportsFailedStartupBuild covers the case main hits when
// the embed service is down at boot: the error is recorded without a rebuild,
// and a nil manager or router reports zero Tools without panicking.
func TestExamplesStateReportsFailedStartupBuild(t *testing.T) {
	f := newFixture(t, nil, nil)
	want := errors.New("embed service is down")
	f.manager.MarkExamplesFailed(want)

	tools, lastErr, at := f.manager.ExamplesState()
	if tools != 0 {
		t.Fatalf("tool count with no router = %d, want 0", tools)
	}
	if !errors.Is(lastErr, want) {
		t.Fatalf("last error = %v, want %v", lastErr, want)
	}
	if at.IsZero() {
		t.Fatal("last attempt is zero after a failed startup build")
	}

	var nilManager *wording.Manager
	if tools, lastErr, at := nilManager.ExamplesState(); tools != 0 || lastErr != nil || !at.IsZero() {
		t.Fatalf("nil manager state = (%d, %v, %v), want zero", tools, lastErr, at)
	}
}
