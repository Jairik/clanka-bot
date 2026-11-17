package voice_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// TestStandaloneStopEndsTheSessionWithoutAFollowUp is the voice half of #501:
// when the reply session reports a suspend, the loop clears the shared history
// and closes the follow-up window instead of listening for another request.
func TestStandaloneStopEndsTheSessionWithoutAFollowUp(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, maxFires: 1}
	stt := &fakeSTT{text: "stop"}
	responder := &fakeResponder{calls: make(chan string, 1), err: voice.ErrSessionSuspended}
	var followUps atomic.Int32
	ended := make(chan struct{}, 1)

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(400 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: stt, Responder: responder,
			FollowUp: func() time.Duration {
				followUps.Add(1)
				return 5 * time.Second
			},
		})
	loop.BindSessionEnd(func() {
		select {
		case ended <- struct{}{}:
		default:
		}
	})
	runLoop(t, loop)

	select {
	case transcript := <-responder.calls:
		if transcript != "stop" {
			t.Fatalf("transcript = %q, want stop", transcript)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop turn never reached the responder")
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the suspend did not end the session")
	}
	if got := followUps.Load(); got != 0 {
		t.Fatalf("follow-up window opened %d times, want it closed", got)
	}
}
