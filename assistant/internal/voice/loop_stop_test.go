package voice_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
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

// TestStopCommandDuringAReplyCancelsItAndSuspendsTheSession covers #503: a
// stop detection while a reply is in flight cancels the reply like a Barge-in,
// drops the pending Confirmation, ends the session, and starts no new turn.
func TestStopCommandDuringAReplyCancelsItAndSuspendsTheSession(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, name: "clanker", maxFires: 1}
	replyWake := &fakeWake{afterWords: 4, names: []string{"stop"}, maxFires: 1}
	stt := &fakeSTT{text: "tell me a long story"}
	dropper := &fakeDropper{pending: tool.PendingConfirmation{Tool: "mail.send"}}
	responder := newBargeResponder()
	var followUps, cues atomic.Int32
	ended := make(chan struct{}, 1)

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(400 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, ReplyWake: replyWake, STT: stt, Responder: responder,
			Dropper:     dropper,
			WakeWords:   []string{"clanker"},
			StopCommand: "stop",
			StopCue:     func(context.Context) { cues.Add(1) },
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
	case <-responder.speaking:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply never started")
	}
	select {
	case <-responder.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the stop command did not cancel the reply context")
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the stop command did not end the session")
	}
	select {
	case transcript := <-responder.finished:
		t.Fatalf("the stop command started a new turn: %q", transcript)
	case <-time.After(300 * time.Millisecond):
	}
	if got := dropper.count(); got != 1 {
		t.Fatalf("pending Confirmation dropped %d times, want 1", got)
	}
	if got := cues.Load(); got != 1 {
		t.Fatalf("stop cue played %d times, want 1", got)
	}
	if got := followUps.Load(); got != 0 {
		t.Fatalf("follow-up window opened %d times, want it closed", got)
	}
	if opens := stt.openCount(); opens != 1 {
		t.Fatalf("STT opens = %d, want no second capture", opens)
	}
}

// TestStopDetectionWithNoStopModelIsIgnored proves the feature is opt-in: with
// no stop model configured, the detection is handled exactly as it is today
// and the reply carries on.
func TestStopDetectionWithNoStopModelIsIgnored(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, name: "clanker", maxFires: 1}
	replyWake := &fakeWake{afterWords: 4, names: []string{"stop"}, maxFires: 1}
	stt := &fakeSTT{text: "hello"}
	responder := &fakeResponder{block: 300 * time.Millisecond, calls: make(chan string, 1)}
	ended := make(chan struct{}, 1)

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(400 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, ReplyWake: replyWake, STT: stt, Responder: responder,
			WakeWords: []string{"clanker"},
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
		if transcript != "hello" {
			t.Fatalf("transcript = %q", transcript)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reply never finished")
	}
	select {
	case <-ended:
		t.Fatal("an unconfigured stop detection ended the session")
	default:
	}
	if responder.sawCancel() {
		t.Fatal("an unconfigured stop detection cancelled the reply")
	}
}

// TestIdleStopDetectionIsIgnored proves the stop model is reply-only: a stop
// detection on the idle stream (which production never requests) ends nothing,
// plays no cue and opens no STT. The next wake word still starts a turn.
func TestIdleStopDetectionIsIgnored(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 3, names: []string{"stop", "clanker"}, maxFires: 2}
	stt := &fakeSTT{text: "hello"}
	responder := &fakeResponder{calls: make(chan string, 1)}
	var cues, ends atomic.Int32

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(400 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: stt, Responder: responder,
			WakeWords:   []string{"clanker"},
			StopCommand: "stop",
			StopCue:     func(context.Context) { cues.Add(1) },
		})
	loop.BindSessionEnd(func() { ends.Add(1) })
	runLoop(t, loop)

	select {
	case transcript := <-responder.calls:
		if transcript != "hello" {
			t.Fatalf("transcript = %q, want the turn after the idle stop", transcript)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wake word after an idle stop did not start a turn")
	}
	if got := ends.Load(); got != 0 {
		t.Fatalf("session ended %d times for an idle stop, want 0", got)
	}
	if got := cues.Load(); got != 0 {
		t.Fatalf("stop cue played %d times for an idle stop, want 0", got)
	}
	if opens := stt.openCount(); opens != 1 {
		t.Fatalf("STT opens = %d, want only the wake-word turn", opens)
	}
}

// TestReplyWatchUsesReplyWake proves the reply-time stream comes from
// ReplyWake, not Wake: the idle service is opened for the wake word only.
func TestReplyWatchUsesReplyWake(t *testing.T) {
	source := &scriptSource{frames: repeat(loudFrame(), 600)}
	wake := &fakeWake{afterWords: 4, name: "clanker", maxFires: 1}
	replyWake := &fakeWake{afterWords: 4, names: []string{"stop"}, maxFires: 1}
	stt := &fakeSTT{text: "tell me a long story"}
	responder := newBargeResponder()

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(400 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, ReplyWake: replyWake, STT: stt, Responder: responder,
			WakeWords:   []string{"clanker"},
			StopCommand: "stop",
		})
	runLoop(t, loop)

	select {
	case <-responder.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop detection from ReplyWake did not cancel the reply")
	}
	if _, _, fires := wake.snapshot(); fires != 1 {
		t.Fatalf("idle Wake fired %d times, want only the wake word", fires)
	}
	if streams, _, fires := replyWake.snapshot(); streams == 0 || fires != 1 {
		t.Fatalf("ReplyWake streams=%d fires=%d, want it opened during the reply", streams, fires)
	}
}
