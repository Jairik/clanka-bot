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

// A blocked predicate can clear without a queue event, such as a Ring that
// times out while a preview waits; the preview must still start on its own.
func TestQueuedAudioResumesWhenTheBlockedGateClears(t *testing.T) {
	var mu sync.Mutex
	blocked := true
	queue := announce.New(nil, testLogger(), announce.WithPlayback(), announce.WithBlocked(func() bool {
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
	player.waitStarted(t)
}

// A Quiet-hours hold applies to queued audio exactly as it does to speech.
func TestQueuedAudioWaitsForAHold(t *testing.T) {
	queue := announce.New(nil, testLogger(), announce.WithPlayback())
	defer queue.Close()
	player := newPlaybackRecorder()

	queue.Hold()
	if !queue.EnqueuePlay(player.play) {
		t.Fatal("EnqueuePlay rejected an audio item")
	}
	player.expectNoStart(t)

	queue.Release()
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

func TestPreemptCancelsAPlayingPreview(t *testing.T) {
	var mu sync.Mutex
	preempt := false
	queue := announce.New(newRecordingSpeaker(), testLogger(), announce.WithPreempt(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return preempt
	}))
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
	mu.Lock()
	preempt = true
	mu.Unlock()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("preview was not cancelled by the higher-priority Ring")
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

// Close must join the worker even while an opaque gate keeps an item queued,
// so a shutdown never leaves the playback goroutine behind.
func TestCloseStopsABlockedWorker(t *testing.T) {
	queue := announce.New(nil, testLogger(), announce.WithPlayback(), announce.WithBlocked(func() bool { return true }))
	if !queue.EnqueuePlay(func(context.Context) error { return nil }) {
		t.Fatal("EnqueuePlay rejected an audio item")
	}
	closed := make(chan struct{})
	go func() {
		queue.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the blocked worker")
	}
}
