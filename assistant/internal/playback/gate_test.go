package playback_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/playback"
)

// TestRingWaitsForTheCurrentReplySentence is the sentence-boundary contract: a
// Ring arriving mid-sentence waits for that chunk to end, then owns the floor,
// and the reply's next chunk waits for the Ring rather than being cancelled.
func TestRingWaitsForTheCurrentReplySentence(t *testing.T) {
	gate := playback.NewGate()
	ctx := context.Background()
	doneReply, err := gate.BeginReply(ctx)
	if err != nil {
		t.Fatalf("BeginReply: %v", err)
	}

	ringReady := make(chan func(), 1)
	go func() {
		release, err := gate.WaitForReplySentence(ctx)
		if err != nil {
			ringReady <- nil
			return
		}
		ringReady <- release
	}()

	select {
	case <-ringReady:
		t.Fatal("the Ring took the floor before the sentence ended")
	case <-time.After(30 * time.Millisecond):
	}
	if !gate.Speaking() || gate.RingActive() {
		t.Fatalf("speaking=%v ringActive=%v, want a speaking reply and no Ring", gate.Speaking(), gate.RingActive())
	}

	doneReply(true)
	var release func()
	select {
	case release = <-ringReady:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring did not take the floor after the sentence ended")
	}
	if release == nil {
		t.Fatal("the Ring waiter returned an error")
	}
	if !gate.RingActive() {
		t.Fatal("the Ring did not own the floor")
	}

	// The reply's next sentence must block while the Ring is active.
	nextStarted := make(chan struct{})
	go func() {
		done, err := gate.BeginReply(ctx)
		if err != nil {
			close(nextStarted)
			return
		}
		close(nextStarted)
		done(true)
	}()
	select {
	case <-nextStarted:
		t.Fatal("the next reply sentence started during the Ring")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case <-nextStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the reply did not resume after the Ring ended")
	}
}

// TestRingWaitsForTheWholeSentenceNotTheFirstClause is why the chunker's first
// clause is not a sentence boundary: a Ring arriving there lets the rest of the
// sentence finish first.
func TestRingWaitsForTheWholeSentenceNotTheFirstClause(t *testing.T) {
	gate := playback.NewGate()
	ctx := context.Background()
	doneClause, err := gate.BeginReply(ctx)
	if err != nil {
		t.Fatalf("BeginReply: %v", err)
	}
	ringReady := make(chan func(), 1)
	go func() {
		release, err := gate.WaitForReplySentence(ctx)
		if err != nil {
			ringReady <- nil
			return
		}
		ringReady <- release
	}()
	time.Sleep(10 * time.Millisecond)

	// The clause ends but the sentence does not: the Ring must keep waiting.
	doneClause(false)
	select {
	case <-ringReady:
		t.Fatal("the Ring took the floor at a clause boundary")
	case <-time.After(30 * time.Millisecond):
	}
	// The continuation unit is allowed even though the Ring is waiting.
	doneRest, err := gate.BeginReply(ctx)
	if err != nil {
		t.Fatalf("continuation BeginReply: %v", err)
	}
	doneRest(true)
	select {
	case release := <-ringReady:
		if release == nil {
			t.Fatal("the Ring waiter failed")
		}
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring did not take the floor at the sentence end")
	}
}

// TestRingTakesAnIdleFloorImmediately keeps a Ring prompt when no reply is
// speaking.
func TestRingTakesAnIdleFloorImmediately(t *testing.T) {
	gate := playback.NewGate()
	release, err := gate.WaitForReplySentence(context.Background())
	if err != nil {
		t.Fatalf("WaitForReplySentence: %v", err)
	}
	if !gate.RingActive() {
		t.Fatal("the Ring did not take the idle floor")
	}
	release()
	if gate.RingActive() {
		t.Fatal("release left the Ring active")
	}
}

// TestWaitingRingBlocksNewSentencesEvenBeforeTheCurrentOneEnds proves the
// reply cannot starve a waiting Ring with a stream of quick chunks.
func TestWaitingRingBlocksNewSentencesEvenBeforeTheCurrentOneEnds(t *testing.T) {
	gate := playback.NewGate()
	ctx := context.Background()
	doneFirst, err := gate.BeginReply(ctx)
	if err != nil {
		t.Fatalf("BeginReply: %v", err)
	}
	waiting := make(chan struct{})
	go func() {
		release, err := gate.WaitForReplySentence(ctx)
		if err == nil {
			release()
		}
		close(waiting)
	}()
	// Give the waiter a moment to register, then end the sentence and try a
	// new one immediately; the pending Ring must win.
	time.Sleep(10 * time.Millisecond)
	doneFirst(true)
	blocked := make(chan struct{})
	go func() {
		done, err := gate.BeginReply(ctx)
		if err == nil {
			done(true)
		}
		close(blocked)
	}()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting Ring did not run")
	}
}

// TestContextCancelsBothWaiters keeps a cancelled turn or shutdown from
// leaking a goroutine.
func TestContextCancelsBothWaiters(t *testing.T) {
	gate := playback.NewGate()
	ctx, cancel := context.WithCancel(context.Background())
	done, err := gate.BeginReply(context.Background())
	if err != nil {
		t.Fatalf("BeginReply: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := gate.WaitForReplySentence(ctx)
		result <- err
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("ring wait error = %v, want context.Canceled", err)
	}
	done(true)

	closed := playback.NewGate()
	closed.Close()
	if _, err := closed.BeginReply(context.Background()); !errors.Is(err, playback.ErrClosed) {
		t.Fatalf("closed BeginReply error = %v, want ErrClosed", err)
	}
	if _, err := closed.WaitForReplySentence(context.Background()); !errors.Is(err, playback.ErrClosed) {
		t.Fatalf("closed WaitForReplySentence error = %v, want ErrClosed", err)
	}
}

// fakeLineSpeaker blocks one announcement line until released.
type fakeLineSpeaker struct {
	started chan struct{}
	release chan struct{}
}

func (s *fakeLineSpeaker) SayText(ctx context.Context, _ string) error {
	close(s.started)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestGatedSpeakerObeysRingPriority proves an Announcement line waits behind a
// Ring and a Ring waits for the line already speaking.
func TestGatedSpeakerObeysRingPriority(t *testing.T) {
	gate := playback.NewGate()
	inner := &fakeLineSpeaker{started: make(chan struct{}), release: make(chan struct{})}
	speaker := playback.NewGatedSpeaker(inner, gate)

	// A Ring owns the floor: the announcement must not start.
	ringRelease, err := gate.WaitForReplySentence(context.Background())
	if err != nil {
		t.Fatalf("ring wait: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- speaker.SayText(context.Background(), "Reminder") }()
	select {
	case <-inner.started:
		t.Fatal("the announcement started while a Ring owned the floor")
	case <-time.After(30 * time.Millisecond):
	}
	ringRelease()
	select {
	case <-inner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the announcement did not start after the Ring")
	}

	// A Ring arriving now waits for the announcement line to finish.
	ringReady := make(chan func(), 1)
	go func() {
		release, err := gate.WaitForReplySentence(context.Background())
		if err != nil {
			ringReady <- nil
			return
		}
		ringReady <- release
	}()
	select {
	case <-ringReady:
		t.Fatal("the Ring took the floor mid-announcement")
	case <-time.After(30 * time.Millisecond):
	}
	close(inner.release)
	if err := <-done; err != nil {
		t.Fatalf("announcement: %v", err)
	}
	select {
	case release := <-ringReady:
		if release == nil {
			t.Fatal("ring waiter failed")
		}
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring did not get the floor after the announcement")
	}
}

// TestNilGateIsSafe keeps text-only and pre-#52 wiring free of nil checks.
func TestNilGateIsSafe(t *testing.T) {
	var gate *playback.Gate
	if gate.Speaking() || gate.RingActive() {
		t.Fatal("a nil gate reports activity")
	}
	done, err := gate.BeginReply(context.Background())
	if err != nil {
		t.Fatalf("nil BeginReply: %v", err)
	}
	done(true)
	release, err := gate.WaitForReplySentence(context.Background())
	if err != nil {
		t.Fatalf("nil Wait: %v", err)
	}
	release()
	gate.Close()
}
