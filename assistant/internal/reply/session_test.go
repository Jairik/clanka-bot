package reply_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// eventLog records playback and streaming in the order they happen.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// logSink records a filler playback in the shared log.
type logSink struct{ log *eventLog }

func (s logSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return logWriter{log: s.log}, nil
}

type logWriter struct{ log *eventLog }

func (w logWriter) Write(p []byte) (int, error) {
	w.log.add("filler-audio")
	return len(p), nil
}

func (w logWriter) Close() error { return nil }

// fakeSpeaker records every phrase it is asked to speak.
type fakeSpeaker struct {
	mu     sync.Mutex
	said   []string
	saidCh chan string
	err    error
}

func newFakeSpeaker() *fakeSpeaker {
	return &fakeSpeaker{saidCh: make(chan string, 8)}
}

func (f *fakeSpeaker) SayText(_ context.Context, text string) error {
	f.mu.Lock()
	f.said = append(f.said, text)
	f.mu.Unlock()
	f.saidCh <- text
	return f.err
}

func (f *fakeSpeaker) phrases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.said...)
}

// fakePipeline stands in for the routing pipeline at the session seam.
type fakePipeline struct {
	handle func(events pipeline.Events) pipeline.Reply
}

func (f *fakePipeline) HandleEvents(_ context.Context, _ tool.Request, _ string, events pipeline.Events) pipeline.Reply {
	return f.handle(events)
}

func testFiller() *speech.Filler {
	return &speech.Filler{
		Text:   "One moment.",
		Format: audio.Format{Rate: 24000, Width: 2, Channels: 1},
		PCM:    []byte{1, 2, 3, 4},
	}
}

func request() tool.Request {
	return tool.Request{ID: "test", Device: tool.DevicePi}
}

func TestAgentAnswerStartsPlayingBeforeGenerationEnds(t *testing.T) {
	log := &eventLog{}
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		events.Delta("The forecast is fine today, but it could rain later.")
		// Generation is still running; the first chunk must already be playing.
		select {
		case chunk := <-speaker.saidCh:
			log.add("streamed:" + chunk)
		case <-time.After(2 * time.Second):
			t.Error("reply audio did not start before generation finished")
		}
		events.Delta(" It is sunny for now.")
		return pipeline.Reply{Text: "The forecast is fine today, but it could rain later. It is sunny for now."}
	}

	session := reply.New(routed, speaker, logSink{log}, testFiller())
	result, err := session.Ask(context.Background(), request(), "will it rain")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Text != "The forecast is fine today, but it could rain later. It is sunny for now." {
		t.Fatalf("reply = %q", result.Text)
	}
	want := []string{"The forecast is fine today,", "but it could rain later.", "It is sunny for now."}
	if got := speaker.phrases(); !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	// The cached filler plays first, then the streamed chunk.
	wantLog := []string{"filler-audio", "streamed:The forecast is fine today,"}
	if got := log.all(); !equal(got, wantLog) {
		t.Fatalf("order = %q, want %q", got, wantLog)
	}
}

func TestAgentFillerPlaysOncePerRequest(t *testing.T) {
	log := &eventLog{}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		events.AgentPath()
		events.Delta("It is sunny.")
		return pipeline.Reply{Text: "It is sunny."}
	}

	session := reply.New(routed, newFakeSpeaker(), logSink{log}, testFiller())
	if _, err := session.Ask(context.Background(), request(), "will it rain"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := log.all(); !equal(got, []string{"filler-audio"}) {
		t.Fatalf("filler played %q, want exactly one", got)
	}
}

func TestFastPathReplyIsSpokenWithoutAFiller(t *testing.T) {
	log := &eventLog{}
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{Text: "It's 3:04 PM.", Path: tool.PathControlPhrase}
	}

	session := reply.New(routed, speaker, logSink{log}, testFiller())
	if _, err := session.Ask(context.Background(), request(), "what time is it"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := speaker.phrases(); !equal(got, []string{"It's 3:04 PM."}) {
		t.Fatalf("spoken = %q", got)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("filler played on the Fast path: %q", got)
	}
}

func TestPlaybackFailureReturnsTheReplyAndTheError(t *testing.T) {
	speaker := newFakeSpeaker()
	speaker.err = errors.New("tts is down")
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{Text: "It is sunny."}
	}

	session := reply.New(routed, speaker, logSink{&eventLog{}}, testFiller())
	result, err := session.Ask(context.Background(), request(), "will it rain")
	if err == nil {
		t.Fatal("Ask succeeded, want the playback error")
	}
	if result.Text != "It is sunny." {
		t.Fatalf("reply = %q, want the text kept", result.Text)
	}
}

func TestSessionWithoutASpeakerIsTextOnly(t *testing.T) {
	log := &eventLog{}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		if events.Delta != nil {
			events.Delta("It is sunny.")
		}
		return pipeline.Reply{Text: "It is sunny."}
	}

	session := reply.New(routed, nil, logSink{log}, testFiller())
	result, err := session.Ask(context.Background(), request(), "will it rain")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Text != "It is sunny." || len(log.all()) != 0 {
		t.Fatalf("reply = %q, log = %q", result.Text, log.all())
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
