package voice_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// fakeDropper stands in for the pipeline's pending-Confirmation slot.
type fakeDropper struct {
	mu      sync.Mutex
	pending tool.PendingConfirmation
	calls   int
}

func (d *fakeDropper) DropPending() (tool.PendingConfirmation, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	dropped := d.pending
	d.pending = tool.PendingConfirmation{}
	return dropped, dropped.Tool != ""
}

func (d *fakeDropper) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// bargeResponder holds its first reply until the request is cancelled, then
// answers later requests at once. The channels let the test observe the
// interruption without racing the loop.
type bargeResponder struct {
	mu         sync.Mutex
	started    int
	speaking   chan struct{}
	cancelled  chan struct{}
	finished   chan string
	once       sync.Once
	cancelOnce sync.Once
}

func newBargeResponder() *bargeResponder {
	return &bargeResponder{
		speaking:  make(chan struct{}),
		cancelled: make(chan struct{}),
		finished:  make(chan string, 4),
	}
}

func (r *bargeResponder) Respond(ctx context.Context, req voice.Request) error {
	r.mu.Lock()
	r.started++
	first := r.started == 1
	r.mu.Unlock()

	r.once.Do(func() { close(r.speaking) })
	if first {
		<-ctx.Done()
		r.cancelOnce.Do(func() { close(r.cancelled) })
		return ctx.Err()
	}
	r.finished <- req.Transcript
	return nil
}

func TestBargeInStopsTheReplyDropsThePendingAndStartsAFreshTurn(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 400)...)}
	// Two firings only: the first opens the request, the second is the barge-in.
	// The third turn's watch stream stays silent so the loop settles.
	wake := &fakeWake{afterWords: 4, maxFires: 2}
	stt := &fakeSTT{text: "what time is it"}
	player := &fakePlayer{}
	dropper := &fakeDropper{pending: tool.PendingConfirmation{Tool: "mail.send"}}
	responder := newBargeResponder()

	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

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
		Dropper:   dropper,
		Logger:    logger,
	})
	runLoop(t, loop)

	select {
	case <-responder.speaking:
	case <-time.After(5 * time.Second):
		t.Fatal("the first reply never started")
	}
	select {
	case <-responder.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the barge-in did not cancel the reply context")
	}
	select {
	case transcript := <-responder.finished:
		if transcript != "what time is it" {
			t.Fatalf("second turn transcript = %q", transcript)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not start a fresh turn after the barge-in")
	}

	if plays := player.plays.Load(); plays != 2 {
		t.Fatalf("chime plays = %d, want 2 (the new turn chimes again)", plays)
	}
	if opens := stt.openCount(); opens != 2 {
		t.Fatalf("STT opens = %d, want a fresh capture for the new turn", opens)
	}
	if got := dropper.count(); got != 1 {
		t.Fatalf("pending Confirmation dropped %d times, want 1", got)
	}
	if text := logs.String(); !strings.Contains(text, "voice: barge-in") {
		t.Fatalf("no barge-in log line in %q", text)
	}
	if text := logs.String(); !strings.Contains(text, "stop_ms") {
		t.Fatalf("the barge-in log has no stop delay in %q", text)
	}
}

func TestWakeStaysLiveSoTheReplyCanBeInterrupted(t *testing.T) {
	// The wake stream is closed before the reply and a fresh one is opened for
	// playback: detection is no longer muted while the Assistant speaks.
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 120)...)}
	wake := &fakeWake{afterWords: 4, maxFires: 1}
	openAtReply := make(chan int, 1)
	responder := &fakeResponder{
		calls: make(chan string, 1),
		onCall: func(context.Context) {
			_, open, _ := wake.snapshot()
			openAtReply <- open
		},
	}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: source, Wake: wake, STT: &fakeSTT{text: "hello"}, Responder: responder})
	runLoop(t, loop)

	select {
	case open := <-openAtReply:
		if open != 1 {
			t.Fatalf("wake streams open at reply = %d, want 1 listening for a barge-in", open)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reply never started")
	}
}
