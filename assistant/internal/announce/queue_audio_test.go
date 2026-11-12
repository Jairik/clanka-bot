package announce_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
)

// playbackRecorder records sound items separately from spoken lines.
type playbackRecorder struct {
	mu       sync.Mutex
	played   int
	canceled int
	started  chan struct{}
	done     chan struct{}
}

func newPlaybackRecorder() *playbackRecorder {
	return &playbackRecorder{started: make(chan struct{}, 8), done: make(chan struct{}, 8)}
}

func (p *playbackRecorder) play(context.Context) error {
	p.mu.Lock()
	p.played++
	p.mu.Unlock()
	p.started <- struct{}{}
	p.done <- struct{}{}
	return nil
}

func (p *playbackRecorder) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("queued audio did not start")
	}
}

func (p *playbackRecorder) expectNoStart(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
		t.Fatal("queued audio started while it should wait")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestQueuedAudioWaitsForTheReply(t *testing.T) {
	queue := announce.New(newRecordingSpeaker(), testLogger())
	defer queue.Close()
	player := newPlaybackRecorder()

	queue.TurnStarted()
	if !queue.EnqueuePlay(player.play) {
		t.Fatal("EnqueuePlay rejected an audio item")
	}
	player.expectNoStart(t)

	queue.TurnFinished()
	player.waitStarted(t)
}

func TestQueuedAudioWaitsForABlockedConfirmation(t *testing.T) {
	var mu sync.Mutex
	blocked := true
	queue := announce.New(newRecordingSpeaker(), testLogger(), announce.WithBlocked(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return blocked
	}))
	defer queue.Close()
	player := newPlaybackRecorder()

	if !queue.EnqueuePlay(player.play) {
		t.Fatal("EnqueuePlay rejected an audio item")
	}
	player.expectNoStart(t)

	mu.Lock()
	blocked = false
	mu.Unlock()
	// Resolving the Confirmation is a reply turn; TurnFinished wakes the queue.
	queue.TurnFinished()
	player.waitStarted(t)
}

func TestTurnStartedCancelsAPlayingPreview(t *testing.T) {
	queue := announce.New(newRecordingSpeaker(), testLogger())
	defer queue.Close()

	started := make(chan struct{})
	canceled := make(chan struct{})
	if !queue.EnqueuePlay(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}) {
		t.Fatal("EnqueuePlay rejected an audio item")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("preview did not start")
	}
	queue.TurnStarted()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("preview was not cancelled by Barge-in")
	}
}

func TestNilSpeakerWithPlaybackRunsAudioOnly(t *testing.T) {
	queue := announce.New(nil, testLogger(), announce.WithPlayback())
	defer queue.Close()

	if queue.Enqueue("Nobody hears this.") {
		t.Fatal("Enqueue accepted text without a speaker")
	}
	player := newPlaybackRecorder()
	if !queue.EnqueuePlay(player.play) {
		t.Fatal("EnqueuePlay rejected audio with WithPlayback")
	}
	player.waitStarted(t)
}

func TestEnqueuePlayRejectedAfterClose(t *testing.T) {
	queue := announce.New(newRecordingSpeaker(), testLogger())
	queue.Close()
	if queue.EnqueuePlay(func(context.Context) error { return nil }) {
		t.Fatal("EnqueuePlay accepted an item after Close")
	}
}
