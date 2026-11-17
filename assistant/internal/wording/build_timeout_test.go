package wording_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// TestExamplesRebuildTimeoutKeepsOldVectorsAndSchedulesRetry proves that when an
// embedder blocks until its context deadline, a 50 ms build timeout unblocks
// Manager.Wait, preserves the live vectors, and schedules a retry on backoff.
func TestExamplesRebuildTimeoutKeepsOldVectorsAndSchedulesRetry(t *testing.T) {
	f, router, embedder := newSemanticFixtureWith(t,
		wording.WithBuildTimeout(50*time.Millisecond),
		wording.WithRetryBackoff(10*time.Millisecond, 40*time.Millisecond),
	)
	ctx := context.Background()

	// Initial vectors route properly.
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("initial route = %q, want test.clock", got.Tool)
	}

	// Make the fake embedder block until ctx is done.
	embedder.setBlockUntilCtx(true)
	before := embedder.callCount()

	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the new time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Manager.Wait must return once the 50 ms build timeout cancels the blocked embed.
	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Unblock the fake embedder so queries and retries can proceed.
	embedder.setBlockUntilCtx(false)

	// Previous vectors remain live.
	if got := router.Match(ctx, "what time is it"); got.Tool != "test.clock" {
		t.Fatalf("old vectors stopped routing after timeout: %q", got.Tool)
	}
	if got := router.Match(ctx, "tell me the new time"); got.Tool != "" {
		t.Fatalf("unreleased wording routed after failed attempt: %q", got.Tool)
	}

	// A retry was scheduled on the backoff timer and now succeeds once the embedder unblocks.
	waitFor(t, "retry to run and swap new vectors", func() bool {
		return embedder.callCount() > before+1 && router.Match(ctx, "tell me the new time").Tool == "test.clock"
	})
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("replaced example still routes after retry swap: %q", got.Tool)
	}
}

// TestExamplesRebuildCloseReturnsPromptlyWhileBlocked proves that Manager.Close
// cancels an in-flight blocked rebuild and returns promptly without waiting for
// the full build timeout.
func TestExamplesRebuildCloseReturnsPromptlyWhileBlocked(t *testing.T) {
	f, _, embedder := newSemanticFixtureWith(t,
		wording.WithBuildTimeout(10*time.Second),
	)
	ctx := context.Background()

	gate := embedder.gateNext()
	before := embedder.callCount()

	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the new time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	embedder.waitCalls(t, before+1)

	// The worker is now blocked inside Embed waiting on gate. Close cancels
	// the worker context, which must cancel the build context and join promptly.
	closed := make(chan struct{})
	go func() {
		f.manager.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close did not return promptly while rebuild was blocked")
	}
	close(gate)
}

// TestExamplesRebuildFinishingInsideBudgetSwaps proves that a rebuild that
// completes within the build timeout budget successfully installs the new vectors.
func TestExamplesRebuildFinishingInsideBudgetSwaps(t *testing.T) {
	f, router, _ := newSemanticFixtureWith(t,
		wording.WithBuildTimeout(200*time.Millisecond),
	)
	ctx := context.Background()

	if _, err := f.settings.Set(ctx, wording.ExamplesKey("test.clock"), []string{"tell me the new time"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := f.manager.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := router.Match(ctx, "tell me the new time"); got.Tool != "test.clock" {
		t.Fatalf("new wording did not route after swap: %q", got.Tool)
	}
	if got := router.Match(ctx, "what time is it"); got.Tool != "" {
		t.Fatalf("old phrase still routed after swap: %q", got.Tool)
	}
}
