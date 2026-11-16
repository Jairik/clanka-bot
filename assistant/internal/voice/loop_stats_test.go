package voice_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// TestLoopCountsNoSpeechFalseWake drives a real loop with a fake wake that
// produces no transcript. It must bump the model's wake and no-speech
// counters and leave the unroutable counter (owned by the observer fan-out)
// alone. This is the in-loop half of #299's measurement surface.
func TestLoopCountsNoSpeechFalseWake(t *testing.T) {
	stats := voice.NewStats()
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, name: "okay_nabu", maxFires: 1}
	stt := &fakeSTT{text: "   "}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: source, Wake: wake, STT: stt, Responder: responder, Stats: stats})
	runLoop(t, loop)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stt.finishCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stt.finishCount() == 0 {
		t.Fatal("STT never finalised the no-speech request")
	}
	got := stats.Snapshot()["okay_nabu"]
	if got.Wakes != 1 || got.NoSpeech != 1 || got.Unroutable != 0 {
		t.Fatalf("counters = %+v, want 1 wake, 1 no-speech, 0 unroutable", got)
	}
}
