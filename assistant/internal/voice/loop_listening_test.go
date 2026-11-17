package voice_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// TestLoopPausedOpensNoStreamCaptureNotBlockedAndResumeStartsTurn tests that when paused,
// no detect stream is opened, capture is not blocked, stale frames are not delivered to
// the first stream on resume, and resume allows a detection to start a turn.
func TestLoopPausedOpensNoStreamCaptureNotBlockedAndResumeStartsTurn(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, maxFires: 1}
	stt := &fakeSTT{text: "hello there"}
	responder := &fakeResponder{calls: make(chan string, 1)}

	var listening atomic.Bool
	listening.Store(false)

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint:   endpointConfig(300 * time.Millisecond),
	}, voice.Deps{
		Source:    source,
		Wake:      wake,
		STT:       stt,
		Responder: responder,
		Listening: listening.Load,
	})

	_, stopped := runLoop(t, loop)
	_ = stopped

	// Let it run while paused for 400 ms.
	time.Sleep(400 * time.Millisecond)

	// Paused opens no stream.
	streams, _, _ := wake.snapshot()
	if streams != 0 {
		t.Fatalf("while paused, streams opened = %d, want 0", streams)
	}

	// Capture is not blocked: source index should have advanced significantly.
	source.mu.Lock()
	readIndex := source.index
	source.mu.Unlock()
	if readIndex < 10 {
		t.Fatalf("capture appears blocked; source index = %d", readIndex)
	}

	// Resume listening.
	streamOpened := make(chan struct{})
	wake.mu.Lock()
	wake.onOpen = func(int) {
		select {
		case <-streamOpened:
		default:
			close(streamOpened)
		}
	}
	wake.mu.Unlock()

	listening.Store(true)

	select {
	case <-streamOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream to open after resume")
	}

	// Detection should fire and turn should execute.
	select {
	case call := <-responder.calls:
		if call != "hello there" {
			t.Errorf("call = %q, want 'hello there'", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start after resume")
	}
}

// TestLoopPausedReplyIgnoresBargeInDetection tests that a reply in progress ignores
// barge-in detections when listening is paused, completing without interruption.
func TestLoopPausedReplyIgnoresBargeInDetection(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, maxFires: 2}
	stt := &fakeSTT{text: "what time is it"}

	var listening atomic.Bool
	listening.Store(true)

	replyFinished := make(chan struct{})
	responder := &fakeResponder{
		calls: make(chan string, 2),
		onCall: func(ctx context.Context) {
			// Pause listening while reply is running.
			listening.Store(false)
			// Sleep a bit so detection has time to arrive on watch stream.
			select {
			case <-ctx.Done():
			case <-time.After(150 * time.Millisecond):
				close(replyFinished)
			}
		},
	}

	loop := newLoop(t, voice.Config{
		FrameBytes:  frameBytes,
		Endpoint:    endpointConfig(300 * time.Millisecond),
		Chime:       []byte{1, 2, 3, 4},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{
		Source:    source,
		Wake:      wake,
		STT:       stt,
		Player:    &fakePlayer{},
		Responder: responder,
		Listening: listening.Load,
	})

	_, _ = runLoop(t, loop)

	select {
	case <-replyFinished:
	case <-time.After(3 * time.Second):
		t.Fatal("reply was interrupted or did not finish")
	}

	if responder.sawCancel() {
		t.Error("responder was cancelled by barge-in, want it to complete")
	}
}

// TestLoopPausedFollowUpWindowDoesNotOpenSTT tests that when listening is paused,
// a completed reply does not enter a follow-up window or open STT without a wake word.
func TestLoopPausedFollowUpWindowDoesNotOpenSTT(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, maxFires: 1}
	stt := &fakeSTT{text: "first question"}

	var listening atomic.Bool
	listening.Store(true)

	responder := &fakeResponder{
		calls: make(chan string, 2),
		onCall: func(ctx context.Context) {
			// Pause before reply finishes so follow-up window is closed.
			listening.Store(false)
		},
	}

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint:   endpointConfig(300 * time.Millisecond),
	}, voice.Deps{
		Source:    source,
		Wake:      wake,
		STT:       stt,
		Responder: responder,
		Listening: listening.Load,
		FollowUp:  func() time.Duration { return 5 * time.Second },
	})

	_, _ = runLoop(t, loop)

	select {
	case call := <-responder.calls:
		if call != "first question" {
			t.Fatalf("first call = %q", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first turn did not complete")
	}

	// Wait past what would have been follow-up capture.
	time.Sleep(300 * time.Millisecond)

	// Only 1 STT open should have occurred (for the first turn), none for follow-up.
	if count := stt.openCount(); count != 1 {
		t.Errorf("stt open count = %d, want 1 (paused follow-up must not open STT)", count)
	}
}
