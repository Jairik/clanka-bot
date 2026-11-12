package reply_test

import (
	"context"
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

// stampSink records when each sound starts playing. Only the cached Filler
// reaches the sink: the fake speaker never opens one.
type stampSink struct {
	mu    sync.Mutex
	opens []time.Time
}

func (s *stampSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens = append(s.opens, time.Now())
	return discardWriter{}, nil
}

func (s *stampSink) times() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.opens...)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) Close() error                { return nil }

// slowFastPath answers on the Fast path only after the delay, like a Tool that
// waits on the network.
func slowFastPath(delay time.Duration, text string) *fakePipeline {
	routed := &fakePipeline{}
	routed.handle = func(pipeline.Events) pipeline.Reply {
		time.Sleep(delay)
		return pipeline.Reply{Text: text, Path: tool.PathFast}
	}
	return routed
}

func TestSlowFastPathPlaysOneFillerOneAndAHalfSecondsAfterTheEndpoint(t *testing.T) {
	sink := &stampSink{}
	speaker := newFakeSpeaker()
	session := reply.New(slowFastPath(1200*time.Millisecond, "It is three o four."), speaker, sink, testFiller())

	// STT finalised 800 ms after the endpointer fired. The cue belongs to the
	// endpointer, so it must not wait another full 1.5 s from the Ask call.
	ended := time.Now().Add(-800 * time.Millisecond)
	asked := time.Now()
	if _, err := session.AskFrom(context.Background(), request(), "what time is it", ended); err != nil {
		t.Fatalf("AskFrom: %v", err)
	}

	opens := sink.times()
	if len(opens) != 1 {
		t.Fatalf("filler played %d times, want exactly one", len(opens))
	}
	if fromEnd := opens[0].Sub(ended); fromEnd < reply.FillerAfter-100*time.Millisecond || fromEnd > reply.FillerAfter+900*time.Millisecond {
		t.Fatalf("filler played %v after the endpoint, want about %v", fromEnd, reply.FillerAfter)
	}
	if fromAsk := opens[0].Sub(asked); fromAsk >= reply.FillerAfter {
		t.Fatalf("filler waited %v from Ask, so the clock ignored the endpoint", fromAsk)
	}
	if got, want := speaker.phrases(), []string{"It is three o four."}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}

func TestAgentFillerIsTheOnlyFillerForTheRequest(t *testing.T) {
	sink := &stampSink{}
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		// The model is slower than the 1.5 s cue, but the Agent path already
		// filled the request's one slot.
		time.Sleep(1200 * time.Millisecond)
		events.Delta("It is sunny.")
		return pipeline.Reply{Text: "It is sunny."}
	}
	session := reply.New(routed, speaker, sink, testFiller())

	asked := time.Now()
	ended := asked.Add(-1000 * time.Millisecond) // the cue deadline is 500 ms from now
	if _, err := session.AskFrom(context.Background(), request(), "will it rain", ended); err != nil {
		t.Fatalf("AskFrom: %v", err)
	}

	opens := sink.times()
	if len(opens) != 1 {
		t.Fatalf("filler played %d times, want the Agent path cue only", len(opens))
	}
	if fromAsk := opens[0].Sub(asked); fromAsk > 300*time.Millisecond {
		t.Fatalf("filler played %v after Ask, want the immediate Agent path cue", fromAsk)
	}
}

// blockingPipeline streams one chunk, then keeps generating until the request
// context is cancelled, like an Agent stream cut off mid-answer.
type blockingPipeline struct {
	said chan struct{}
}

func (p *blockingPipeline) HandleEvents(ctx context.Context, _ tool.Request, _ string, events pipeline.Events) pipeline.Reply {
	events.Delta("The forecast is fine today, and")
	close(p.said)
	<-ctx.Done()
	return pipeline.Reply{Text: "The forecast is fine today, and"}
}

func TestCancelledReplyStopsStreamingAndNeverPlaysTheFiller(t *testing.T) {
	sink := &stampSink{}
	speaker := newFakeSpeaker()
	session := reply.New(&blockingPipeline{said: make(chan struct{})}, speaker, sink, testFiller())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.AskFrom(ctx, request(), "will it rain", time.Now())
		done <- err
	}()

	select {
	case <-speaker.saidCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the first chunk was never spoken")
	}
	stopped := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("Ask: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after cancellation")
	}
	if elapsed := time.Since(stopped); elapsed > 700*time.Millisecond {
		t.Fatalf("playback took %v to stop after cancellation", elapsed)
	}
	if got := speaker.phrases(); len(got) != 1 {
		t.Fatalf("spoken = %q, want the single interrupted chunk", got)
	}
	if opens := sink.times(); len(opens) != 0 {
		t.Fatalf("filler played after the request was cancelled")
	}
}

// timedSink records when each sound opened, how many bytes it wrote and when
// it closed, so a test can prove the Filler and the reply never overlap.
type timedSink struct {
	mu     sync.Mutex
	sounds []soundStamp
}

type soundStamp struct {
	opened time.Time
	closed time.Time
	bytes  int
}

func (s *timedSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return &timedWriter{sink: s, opened: time.Now()}, nil
}

func (s *timedSink) stamps() []soundStamp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]soundStamp(nil), s.sounds...)
}

type timedWriter struct {
	sink   *timedSink
	opened time.Time
	bytes  int
}

func (w *timedWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}

func (w *timedWriter) Close() error {
	w.sink.mu.Lock()
	w.sink.sounds = append(w.sink.sounds, soundStamp{opened: w.opened, closed: time.Now(), bytes: w.bytes})
	w.sink.mu.Unlock()
	return nil
}

// slowTTSSpeaker models TTS: text is available immediately, but the first PCM
// chunk is only submitted after ttsDelay.
type slowTTSSpeaker struct {
	sink     speech.Sink
	ttsDelay time.Duration
	mu       sync.Mutex
	audioAt  time.Time
}

func (s *slowTTSSpeaker) SayText(ctx context.Context, _ string) error {
	select {
	case <-time.After(s.ttsDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if hook := speech.FirstAudioHook(ctx); hook != nil {
		hook()
	}
	writer, err := s.sink.Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.audioAt = time.Now()
	s.mu.Unlock()
	if _, err := writer.Write([]byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func (s *slowTTSSpeaker) started() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioAt
}

func TestFillerPlaysWhenTextArrivesBeforeReplyAudio(t *testing.T) {
	inner := &timedSink{}
	playSink := speech.NewSerialSink(inner)
	speaker := &slowTTSSpeaker{sink: playSink, ttsDelay: 1100 * time.Millisecond}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		time.Sleep(1400 * time.Millisecond) // text at 1.4 s
		events.Delta("It is three o four.")
		return pipeline.Reply{Text: "It is three o four.", Path: tool.PathFast}
	}
	session := reply.New(routed, speaker, playSink, testFiller())

	started := time.Now()
	if _, err := session.AskFrom(context.Background(), request(), "what time is it", started); err != nil {
		t.Fatalf("AskFrom: %v", err)
	}

	sounds := inner.stamps()
	if len(sounds) != 2 {
		t.Fatalf("sounds = %d, want the filler and the reply", len(sounds))
	}
	filler, response := sounds[0], sounds[1]
	if fromStart := filler.opened.Sub(started); fromStart < 1400*time.Millisecond || fromStart > 2100*time.Millisecond {
		t.Fatalf("filler started %v after Ask, want about 1.5 s (text had arrived but audio had not)", fromStart)
	}
	if filler.bytes != len(testFiller().PCM) {
		t.Fatalf("first sound wrote %d bytes, want the cached filler", filler.bytes)
	}
	if response.bytes <= filler.bytes {
		t.Fatalf("second sound wrote %d bytes, want the reply PCM", response.bytes)
	}
	if filler.closed.After(response.opened) {
		t.Fatalf("filler (closed %v) overlapped the reply (opened %v)", filler.closed, response.opened)
	}
	if response.opened.Sub(started) < 2400*time.Millisecond {
		t.Fatalf("reply audio started %v after Ask, want after the 2.5 s TTS delay", response.opened.Sub(started))
	}
}

func TestReplyAudioBeforeTheFillerDeadlineSuppressesTheFiller(t *testing.T) {
	inner := &timedSink{}
	playSink := speech.NewSerialSink(inner)
	speaker := &slowTTSSpeaker{sink: playSink, ttsDelay: 100 * time.Millisecond}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.Delta("It is three o four.")
		return pipeline.Reply{Text: "It is three o four.", Path: tool.PathFast}
	}
	session := reply.New(routed, speaker, playSink, testFiller())

	if _, err := session.AskFrom(context.Background(), request(), "what time is it", time.Now()); err != nil {
		t.Fatalf("AskFrom: %v", err)
	}
	sounds := inner.stamps()
	if len(sounds) != 1 {
		t.Fatalf("sounds = %d, want only the reply (no filler)", len(sounds))
	}
	if sounds[0].bytes <= len(testFiller().PCM) {
		t.Fatalf("only sound wrote %d bytes, want reply PCM", sounds[0].bytes)
	}
}
