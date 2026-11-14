package ring_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/playback"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

var integrationFormat = audio.Format{Rate: 16000, Width: 2, Channels: 1}

// taggedSink blocks a writer until its tag is released, so a test can model a
// Ring and a Pi preview contending for the one serial speaker.
type taggedSink struct {
	started chan byte
	ring    chan struct{}
	preview chan struct{}
}

func newTaggedSink() *taggedSink {
	return &taggedSink{started: make(chan byte, 32)}
}

func (s *taggedSink) Open(ctx context.Context, _ audio.Format) (io.WriteCloser, error) {
	return &taggedWriter{sink: s, ctx: ctx}, nil
}

type taggedWriter struct {
	sink *taggedSink
	ctx  context.Context
	once sync.Once
}

func (w *taggedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	tag := p[0]
	w.once.Do(func() {
		select {
		case w.sink.started <- tag:
		default:
		}
	})
	wait := w.sink.ring
	if tag == 2 {
		wait = w.sink.preview
	}
	if wait == nil {
		return len(p), nil
	}
	select {
	case <-wait:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

func (w *taggedWriter) Close() error { return nil }

// taggedClip is a short mono PCM16 clip whose first byte names the player.
func taggedClip(tag byte) sound.Clip {
	pcm := make([]byte, 320)
	for i := range pcm {
		pcm[i] = tag
	}
	return sound.Clip{PCM: pcm, Format: integrationFormat, Duration: 10 * time.Millisecond}
}

func waitTag(t *testing.T, started <-chan byte, want byte) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-started:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("tag %d did not play", want)
		}
	}
}

func expectNoTag(t *testing.T, started <-chan byte, avoid byte, wait time.Duration) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case got := <-started:
			if got == avoid {
				t.Fatalf("tag %d started while it should wait", avoid)
			}
		case <-deadline:
			return
		}
	}
}

// newIntegrationQueue wires the production priority rules: a Ring blocks new
// unprompted audio and preempts a preview that is already playing.
func newIntegrationQueue(registry *focus.Registry) *announce.Queue {
	return announce.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		announce.WithPlayback(),
		announce.WithBlocked(func() bool { return registry.Held(focus.Ring) }),
		announce.WithPreempt(func() bool { return registry.Held(focus.Ring) }),
	)
}

// newIntegrationRing builds an engine that loops a tagged clip through the one
// serial sink until its Focus intent ends it.
func newIntegrationRing(registry *focus.Registry, gate *playback.Gate, serial *speech.SerialSink, gap time.Duration) *ring.Engine {
	return ring.New(ring.Deps{
		Focus:    registry,
		Boundary: gate,
		Settings: func(ring.Kind) ring.Settings {
			return ring.Settings{
				MaxRing:    30 * time.Second,
				Volume:     100,
				Gap:        gap,
				SpeakLabel: ring.SpeakNever,
			}
		},
		Clip: func(ring.Kind) (sound.Clip, bool) { return taggedClip(1), true },
		Play: func(ctx context.Context, clip sound.Clip) error { return clip.Play(ctx, serial) },
	})
}

// TestQueuedPreviewWaitsForRingThenResumes covers the #56 queue behind a #52
// Ring: the preview must not start over the Ring, and it must resume on its own
// when the Ring ends.
func TestQueuedPreviewWaitsForRingThenResumes(t *testing.T) {
	sink := newTaggedSink()
	sink.preview = make(chan struct{})
	serial := speech.NewSerialSink(sink)
	registry := focus.New()
	gate := playback.NewGate()
	queue := newIntegrationQueue(registry)
	defer queue.Close()
	engine := newIntegrationRing(registry, gate, serial, 2*time.Second)

	done := make(chan ring.Result, 1)
	go func() {
		done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Timer, ID: "tea", Label: "tea"})
	}()
	waitTag(t, sink.started, 1)
	if !registry.Held(focus.Ring) {
		t.Fatal("the Ring did not hold Focus")
	}

	preview := taggedClip(2)
	if !queue.EnqueuePlay(func(ctx context.Context) error { return preview.Play(ctx, serial) }) {
		t.Fatal("EnqueuePlay rejected a preview")
	}
	expectNoTag(t, sink.started, 2, 400*time.Millisecond)

	if _, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok {
		t.Fatal("stop did not reach the Ring")
	}
	select {
	case result := <-done:
		if result.EndedBy != "stop" {
			t.Fatalf("Ring ended by %q, want stop", result.EndedBy)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the Ring did not stop")
	}

	// The preview resumes through the blocked-gate recheck without another event.
	waitTag(t, sink.started, 2)
	close(sink.preview)
}

// TestLongPreviewDoesNotDelayTheRing proves the Ring preempts an already
// playing preview rather than waiting behind an arbitrarily long sound.
func TestLongPreviewDoesNotDelayTheRing(t *testing.T) {
	sink := newTaggedSink()
	sink.preview = make(chan struct{})
	serial := speech.NewSerialSink(sink)
	registry := focus.New()
	gate := playback.NewGate()
	queue := newIntegrationQueue(registry)
	defer queue.Close()
	engine := newIntegrationRing(registry, gate, serial, 10*time.Millisecond)

	previewErr := make(chan error, 1)
	preview := taggedClip(2)
	if !queue.EnqueuePlay(func(ctx context.Context) error {
		err := preview.Play(ctx, serial)
		previewErr <- err
		return err
	}) {
		t.Fatal("EnqueuePlay rejected a preview")
	}
	waitTag(t, sink.started, 2)

	done := make(chan ring.Result, 1)
	go func() {
		done <- engine.Ring(context.Background(), ring.Request{Kind: ring.Alarm, ID: "wake"})
	}()
	waitTag(t, sink.started, 1)

	select {
	case err := <-previewErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preview error = %v, want cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the preview was not cancelled by the Ring")
	}

	if _, ok := registry.HandleIntent(context.Background(), focus.Stop); !ok {
		t.Fatal("stop did not reach the Ring")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the Ring did not stop")
	}
}
