package voice_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// fakeAnnouncer records turn boundaries at the voice seam.
type fakeAnnouncer struct {
	started  atomic.Int32
	finished chan struct{}
}

func newFakeAnnouncer() *fakeAnnouncer {
	return &fakeAnnouncer{finished: make(chan struct{}, 8)}
}

func (a *fakeAnnouncer) TurnStarted()  { a.started.Add(1) }
func (a *fakeAnnouncer) TurnFinished() { a.finished <- struct{}{} }

// chimeCheckPlayer records whether the announcement was stopped before the
// wake chime played.
type chimeCheckPlayer struct {
	announcer   *fakeAnnouncer
	beforeChime atomic.Bool
}

func (p *chimeCheckPlayer) Play(context.Context, []byte, audio.Format) error {
	p.beforeChime.Store(p.announcer.started.Load() > 0)
	return nil
}

func TestWakeStopsAnnouncementsBeforeTheChime(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{text: "what time is it"}
	announcer := newFakeAnnouncer()
	player := &chimeCheckPlayer{announcer: announcer}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes:  frameBytes,
		Endpoint:    endpointConfig(500 * time.Millisecond),
		Chime:       []byte{1, 2, 3, 4},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{
		Source:    source,
		Wake:      wake,
		STT:       stt,
		Player:    player,
		Responder: responder,
		Announcer: announcer,
	})
	runLoop(t, loop)

	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	if !player.beforeChime.Load() {
		t.Fatal("the chime played before the Announcement was stopped")
	}
	if got := announcer.started.Load(); got != 1 {
		t.Fatalf("TurnStarted calls = %d, want 1", got)
	}
	select {
	case <-announcer.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never finished at the voice seam")
	}
}
