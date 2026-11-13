package ring_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
)

// Media holds Ring Focus, suppresses TTS, and inherits stop cancellation.
func TestCompletionMediaStop(t *testing.T) {
	registry := focus.New()
	started := make(chan struct{})
	ended := make(chan ring.Result, 1)
	speaker := &recordingSpeaker{}
	engine := ring.New(ring.Deps{Focus: registry, Speaker: speaker, Media: func(ctx context.Context, audio string) error {
		if audio != "song" {
			t.Errorf("audio=%q", audio)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Minute {
			t.Error("missing media deadline")
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}})
	go func() { ended <- engine.Ring(t.Context(), ring.Request{Kind: ring.Timer, Audio: "song"}) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("media did not start")
	}
	registry.HandleIntent(t.Context(), focus.Stop)
	select {
	case result := <-ended:
		if result.EndedBy != "stop" {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel media")
	}
	if len(speaker.all()) != 0 {
		t.Fatal("media spoke TTS")
	}
}

// A media load failure speaks the standard alert even when a Cue is configured.
func TestCompletionMediaFallback(t *testing.T) {
	speaker := &recordingSpeaker{}
	engine := ring.New(ring.Deps{Speaker: speaker, Media: func(context.Context, string) error { return errors.New("load failed") }})
	result := engine.Ring(t.Context(), ring.Request{Kind: ring.Timer, Audio: "missing"})
	if !result.Played || len(speaker.all()) != 1 || speaker.all()[0] != "Your timer is done." {
		t.Fatalf("result=%+v speech=%v", result, speaker.all())
	}
}
