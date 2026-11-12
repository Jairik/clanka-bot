package voice_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// fakeDucker records duck and restore order and whether music was ducked.
type fakeDucker struct {
	mu       sync.Mutex
	ducked   bool
	events   []string
	restored chan struct{}
}

func (d *fakeDucker) Duck(context.Context) (func(), error) {
	d.mu.Lock()
	d.ducked = true
	d.events = append(d.events, "duck")
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.ducked = false
		d.events = append(d.events, "restore")
		d.mu.Unlock()
		d.restored <- struct{}{}
	}, nil
}

func (d *fakeDucker) isDucked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ducked
}

func TestMusicDucksFromTheWakeWordThroughTheReplyAndRestores(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{text: "what time is it"}
	ducker := &fakeDucker{restored: make(chan struct{}, 4)}
	duckedAtReply := make(chan bool, 1)
	responder := &fakeResponder{calls: make(chan string, 1), onCall: func(context.Context) {
		duckedAtReply <- ducker.isDucked()
	}}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(500 * time.Millisecond)}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Responder: responder, Ducker: ducker,
	})
	runLoop(t, loop)

	select {
	case ducked := <-duckedAtReply:
		if !ducked {
			t.Fatal("music was not ducked while the reply was spoken")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	select {
	case <-ducker.restored:
	case <-time.After(2 * time.Second):
		t.Fatal("music was never restored after the turn")
	}
}
