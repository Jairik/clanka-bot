package voice_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

const (
	frameBytes = 1024 // 32 ms of 16 kHz mono PCM16
	frameTime  = 32 * time.Millisecond
)

func loudFrame() []byte {
	pcm := make([]byte, frameBytes)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(8000))
	}
	return pcm
}

func silentFrame() []byte { return make([]byte, frameBytes) }

// scriptSource streams a script of frames, repeating the last one. It paces
// reads so the loop and services interleave like they do with a real mic.
type scriptSource struct {
	frames [][]byte
	mu     sync.Mutex
	index  int
	ctx    context.Context
}

func (s *scriptSource) Open(ctx context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	return s, nil
}

func (s *scriptSource) Read(p []byte) (int, error) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(time.Millisecond):
	}

	s.mu.Lock()
	frame := s.frames[s.index]
	if s.index < len(s.frames)-1 {
		s.index++
	}
	s.mu.Unlock()
	return copy(p, frame), nil
}

func (s *scriptSource) Close() error { return nil }

// stagedSource streams silence until the test stages a script, then plays it
// once and returns to silence. A follow-up fixture stages its speech when the
// loop reaches the state under test, so it never depends on how fast the
// scheduler drains a padding run of frames (#332).
type stagedSource struct {
	ctx context.Context

	mu     sync.Mutex
	script [][]byte
	index  int
}

func (s *stagedSource) Open(ctx context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	return s, nil
}

func (s *stagedSource) Read(p []byte) (int, error) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(time.Millisecond):
	}

	s.mu.Lock()
	frame := silentFrame()
	if s.index < len(s.script) {
		frame = s.script[s.index]
		s.index++
	}
	s.mu.Unlock()
	return copy(p, frame), nil
}

func (s *stagedSource) Close() error { return nil }

// stage appends frames to the script instead of replacing it, so a test can
// deliver one utterance after another without losing the current position.
func (s *stagedSource) stage(frames [][]byte) {
	s.mu.Lock()
	s.script = append(s.script, frames...)
	s.mu.Unlock()
}

// fakeWake fires once per opened stream after the configured number of frames.
type fakeWake struct {
	afterWords int
	// name is the model the fake reports; empty means okay_nabu.
	name string
	// names is an optional per-fire sequence consumed before name, so a test
	// can script a command detection followed by a wake word.
	names []string
	// maxFires caps firings across streams so a test can let the loop settle
	// after the interruptions it means to observe.
	maxFires int
	// onOpen, when set, runs after each stream opens with its 1-based count, so
	// a test can synchronize on the loop going back to waiting for a wake word.
	onOpen func(stream int)

	// loud counts the voiced frames written to any stream: evidence that staged
	// speech reached the loop instead of being dropped at a closed window.
	loud atomic.Int64

	mu          sync.Mutex
	streams     int
	openStreams int
	fires       int
}

func (f *fakeWake) Open(ctx context.Context) (voice.WakeStream, error) {
	f.mu.Lock()
	f.streams++
	f.openStreams++
	stream := f.streams
	f.mu.Unlock()
	if f.onOpen != nil {
		f.onOpen(stream)
	}
	return &fakeWakeStream{owner: f, detections: make(chan voice.Detection, 1)}, nil
}

func (f *fakeWake) loudFrames() int { return int(f.loud.Load()) }

func (f *fakeWake) snapshot() (streams, open, fires int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams, f.openStreams, f.fires
}

type fakeWakeStream struct {
	owner      *fakeWake
	detections chan voice.Detection

	mu     sync.Mutex
	frames int
	closed bool
}

func (s *fakeWakeStream) Write(pcm []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("wake stream is closed")
	}
	s.frames++
	frames := s.frames
	s.mu.Unlock()

	if !isSilent(pcm) {
		s.owner.loud.Add(1)
	}
	if frames == s.owner.afterWords {
		s.owner.mu.Lock()
		if s.owner.maxFires > 0 && s.owner.fires >= s.owner.maxFires {
			s.owner.mu.Unlock()
			return nil
		}
		s.owner.fires++
		name := s.owner.name
		if s.owner.fires <= len(s.owner.names) {
			name = s.owner.names[s.owner.fires-1]
		}
		s.owner.mu.Unlock()
		if name == "" {
			name = "okay_nabu"
		}
		select {
		case s.detections <- voice.Detection{Name: name}:
		default:
		}
	}
	return nil
}

func (s *fakeWakeStream) Detections() <-chan voice.Detection { return s.detections }

func (s *fakeWakeStream) Err() error { return nil }

func (s *fakeWakeStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.owner.mu.Lock()
	s.owner.openStreams--
	s.owner.mu.Unlock()
	return nil
}

// fakeSTT records the frames it receives so tests can inspect endpointing.
type fakeSTT struct {
	text string
	// err, when set, makes Finish fail like a client read-idle timeout.
	err error
	// onWrite, when set, runs after each frame with its 1-based Open number, so
	// a test can synchronize on a capture session starting to record audio.
	onWrite func(stream int)

	mu      sync.Mutex
	frames  [][]byte
	opens   int
	finish  int
	closing bool
}

func (f *fakeSTT) Open(ctx context.Context) (voice.STTStream, error) {
	f.mu.Lock()
	f.opens++
	stream := f.opens
	f.mu.Unlock()
	return &fakeSTTStream{owner: f, stream: stream}, nil
}

func (f *fakeSTT) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func (f *fakeSTT) finishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finish
}

func (f *fakeSTT) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

type fakeSTTStream struct {
	owner  *fakeSTT
	stream int
}

func (s *fakeSTTStream) Write(pcm []byte) error {
	s.owner.mu.Lock()
	s.owner.frames = append(s.owner.frames, append([]byte(nil), pcm...))
	hook := s.owner.onWrite
	s.owner.mu.Unlock()
	if hook != nil {
		hook(s.stream)
	}
	return nil
}

func (s *fakeSTTStream) Finish() (string, error) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.owner.finish++
	return s.owner.text, s.owner.err
}

func (s *fakeSTTStream) Close() error { return nil }

type fakePlayer struct {
	plays atomic.Int32
	block time.Duration
}

func (p *fakePlayer) Play(ctx context.Context, pcm []byte, format audio.Format) error {
	p.plays.Add(1)
	if p.block > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.block):
		}
	}
	return nil
}

// fakeResponder records transcripts and what the wake service looked like when
// the reply started: the question's stream is closed and one playback-time
// stream is open, listening for a Barge-in.
type fakeResponder struct {
	owner  *ownerRef
	block  time.Duration
	calls  chan string
	onCall func(ctx context.Context)

	mu        sync.Mutex
	wakes     []string
	cancelled bool
}

func (r *fakeResponder) wakeWords() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.wakes...)
}

func (r *fakeResponder) sawCancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

type ownerRef struct {
	wake        *fakeWake
	openAtReply atomic.Int32
}

func (r *fakeResponder) Respond(ctx context.Context, req voice.Request) error {
	r.mu.Lock()
	r.wakes = append(r.wakes, req.WakeWord)
	r.mu.Unlock()
	if r.owner != nil {
		_, open, _ := r.owner.wake.snapshot()
		r.owner.openAtReply.Store(int32(open))
	}
	if r.onCall != nil {
		r.onCall(ctx)
	}
	if r.block > 0 {
		<-time.After(r.block)
	}
	r.mu.Lock()
	if ctx.Err() != nil {
		r.cancelled = true
	}
	r.mu.Unlock()
	select {
	case r.calls <- req.Transcript:
	default:
	}
	return ctx.Err()
}

func newLoop(t *testing.T, cfg voice.Config, deps voice.Deps) *voice.Loop {
	t.Helper()
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	loop, err := voice.New(cfg, deps)
	if err != nil {
		t.Fatalf("voice.New: %v", err)
	}
	return loop
}

// runLoop starts the loop and returns a cancel function plus a channel that
// closes when Run returns.
func runLoop(t *testing.T, loop *voice.Loop) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = loop.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("loop did not stop after cancel")
		}
	})
	return cancel, stopped
}

func endpointConfig(silence time.Duration) voice.EndpointConfig {
	return voice.EndpointConfig{Rate: 16000, Silence: silence, ThresholdDBFS: -38, MaxUtterance: 10 * time.Second}
}

func TestLoopChimesOnceAndWatchesForBargeInWhileReplying(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{text: "what time is it"}
	player := &fakePlayer{}
	owner := &ownerRef{wake: wake}
	responder := &fakeResponder{owner: owner, calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes:  frameBytes,
		Endpoint:    endpointConfig(500 * time.Millisecond),
		Chime:       []byte{1, 2, 3, 4},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{Source: source, Wake: wake, STT: stt, Player: player, Responder: responder})
	runLoop(t, loop)

	select {
	case got := <-responder.calls:
		if got != "what time is it" {
			t.Fatalf("transcript = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}

	if plays := player.plays.Load(); plays != 1 {
		t.Fatalf("chime plays = %d, want 1", plays)
	}
	if open := owner.openAtReply.Load(); open != 1 {
		t.Fatalf("wake streams open at reply = %d, want 1 (listening for a barge-in)", open)
	}
	if stt.finishCount() != 1 {
		t.Fatalf("STT Finish calls = %d, want 1", stt.finishCount())
	}
}

func TestLoopUsesTheConfiguredSilenceWindow(t *testing.T) {
	tests := []struct {
		silence      time.Duration
		wantTrailing int
	}{
		{silence: 500 * time.Millisecond, wantTrailing: 16},
		{silence: 300 * time.Millisecond, wantTrailing: 10},
	}

	for _, test := range tests {
		t.Run(test.silence.String(), func(t *testing.T) {
			source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
			wake := &fakeWake{afterWords: 5}
			stt := &fakeSTT{text: "hello"}
			responder := &fakeResponder{calls: make(chan string, 1)}

			loop := newLoop(t, voice.Config{
				FrameBytes: frameBytes,
				Endpoint:   endpointConfig(test.silence),
			}, voice.Deps{Source: source, Wake: wake, STT: stt, Responder: responder})
			runLoop(t, loop)

			select {
			case <-responder.calls:
			case <-time.After(5 * time.Second):
				t.Fatal("no reply")
			}

			frames := stt.received()
			if len(frames) == 0 {
				t.Fatal("STT received no audio")
			}
			trailing := 0
			for i := len(frames) - 1; i >= 0; i-- {
				if !isSilent(frames[i]) {
					break
				}
				trailing++
			}
			if trailing != test.wantTrailing {
				t.Fatalf("trailing silence frames = %d, want %d for a %v window", trailing, test.wantTrailing, test.silence)
			}
		})
	}
}

func TestLoopStaysQuietWithoutAWakeWord(t *testing.T) {
	source := &scriptSource{frames: repeat(silentFrame(), 10)}
	wake := &fakeWake{afterWords: 1 << 30}
	stt := &fakeSTT{text: "should not happen"}
	player := &fakePlayer{}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes:  frameBytes,
		Endpoint:    endpointConfig(500 * time.Millisecond),
		Chime:       []byte{1, 2},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{Source: source, Wake: wake, STT: stt, Player: player, Responder: responder})
	cancel, stopped := runLoop(t, loop)

	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop")
	}

	if plays := player.plays.Load(); plays != 0 {
		t.Fatalf("chime plays = %d, want 0", plays)
	}
	if stt.opens != 0 {
		t.Fatalf("STT opened %d streams without a wake word", stt.opens)
	}
	if _, _, fires := wake.snapshot(); fires != 0 {
		t.Fatalf("wake fired %d times", fires)
	}
}

func TestLoopDoesNotEndpointOnTheChime(t *testing.T) {
	// The chime comes back through the mic, so audio while it plays must not
	// count as the question. With a slow player a buggy loop ends the request
	// during the chime; the correct loop waits for it and then its utterance cap.
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 100)...)}
	wake := &fakeWake{afterWords: 3}
	stt := &fakeSTT{text: "hello"}
	player := &fakePlayer{block: 500 * time.Millisecond}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint: voice.EndpointConfig{
			Rate:          16000,
			Silence:       30 * time.Millisecond,
			ThresholdDBFS: -38,
			MaxUtterance:  600 * time.Millisecond,
		},
		Chime:       []byte{1, 2, 3, 4},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{Source: source, Wake: wake, STT: stt, Player: player, Responder: responder})

	started := time.Now()
	runLoop(t, loop)
	select {
	case <-responder.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	if elapsed := time.Since(started); elapsed < 450*time.Millisecond {
		t.Fatalf("request ended after %v, during the chime", elapsed)
	}
}

func TestLoopSkipsTheReplyWhenNothingWasSaid(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5}
	stt := &fakeSTT{text: "   "}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: source, Wake: wake, STT: stt, Responder: responder})
	runLoop(t, loop)

	select {
	case got := <-responder.calls:
		t.Fatalf("responded with %q for an empty transcript", got)
	case <-time.After(500 * time.Millisecond):
	}
	if stt.finishCount() == 0 {
		t.Fatal("STT never finalised the empty request")
	}
}

// TestLoopRecoversAfterSTTFinalisationTimesOut is #201's loop seam: a Finish
// that returns an idle-timeout error is a failed turn, logged and retried, not
// a wedged session that leaves the assistant deaf.
func TestLoopRecoversAfterSTTFinalisationTimesOut(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 400)...)}
	wake := &fakeWake{afterWords: 5, maxFires: 1}
	stt := &fakeSTT{err: errors.New("wyoming: transcribe: no final transcript within 20s: i/o timeout")}
	responder := &fakeResponder{calls: make(chan string, 1)}
	logs := &lockedBuffer{}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{
			Source: source, Wake: wake, STT: stt, Responder: responder,
			Logger: slog.New(slog.NewTextHandler(logs, nil)),
		})
	runLoop(t, loop)

	// A failed session sleeps 500ms and then opens a fresh wake stream.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if streams, _, _ := wake.snapshot(); stt.finishCount() > 0 && streams >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if streams, _, _ := wake.snapshot(); streams < 2 {
		t.Fatalf("loop did not return to listening after the STT timeout:\n%s", logs.String())
	}
	if got := logs.String(); !strings.Contains(got, "voice: session") || !strings.Contains(got, "transcribe") {
		t.Fatalf("STT timeout was not logged as a failed session:\n%s", got)
	}
	select {
	case got := <-responder.calls:
		t.Fatalf("responded %q after a failed finalisation", got)
	default:
	}
}

func TestLoopStopsWhenTheReplyContextIsCancelled(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 3}
	stt := &fakeSTT{text: "ask the agent something long"}
	started := make(chan struct{}, 1)
	responder := &fakeResponder{calls: make(chan string, 1), onCall: func(ctx context.Context) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done() // hold the reply until the session is cancelled
	}}

	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: source, Wake: wake, STT: stt, Responder: responder})
	cancel, stopped := runLoop(t, loop)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reply never started")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop")
	}
}

func repeat(frame []byte, n int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		frames[i] = append([]byte(nil), frame...)
	}
	return frames
}

func isSilent(pcm []byte) bool {
	for i := 0; i < len(pcm); i++ {
		if pcm[i] != 0 {
			return false
		}
	}
	return true
}

// utteranceFrames is one spoken question: enough voiced audio that a wake
// detection, the loop's select and the session's pre-capture gate cannot drop
// all of it under load, then trailing silence to endpoint it.
func utteranceFrames() [][]byte {
	return append(repeat(loudFrame(), 40), repeat(silentFrame(), 40)...)
}

// waitForSignal waits for a loop transition a fixture exposes, or fails.
func waitForSignal(t *testing.T, signal <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// waitForReply waits for one completed reply, or fails.
func waitForReply(t *testing.T, calls <-chan string, msg string) string {
	t.Helper()
	select {
	case transcript := <-calls:
		return transcript
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
	return ""
}

// flakySource fails its first Open, like Pulse not being ready at container start.
type flakySource struct {
	inner *scriptSource
	mu    sync.Mutex
	opens int
}

func (f *flakySource) Open(ctx context.Context) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opens++
	n := f.opens
	f.mu.Unlock()
	if n == 1 {
		return nil, errors.New("pulse not ready")
	}
	return f.inner.Open(ctx)
}

func TestLoopRetriesAFailedCaptureSource(t *testing.T) {
	inner := &scriptSource{frames: repeat(loudFrame(), 40)}
	source := &flakySource{inner: inner}
	wake := &fakeWake{afterWords: 5}
	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint:   endpointConfig(500 * time.Millisecond),
	}, voice.Deps{Source: source, Wake: wake, STT: &fakeSTT{}, Responder: &fakeResponder{}})
	cancel, stopped := runLoop(t, loop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if streams, _, _ := wake.snapshot(); streams > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if streams, _, _ := wake.snapshot(); streams == 0 {
		t.Fatal("the wake stream never opened after the capture retry")
	}
	select {
	case <-stopped:
		t.Fatal("the loop exited on a recoverable capture failure")
	default:
	}
	cancel()
}

// lockedBuffer collects JSON logs from the loop goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLoopStagesCorrelateUnderOneRequestID is the #27 seam: endpointing, STT
// finalise and the reply hand-off share one request ID, and the timeline
// starts at the last voiced frame.
func TestLoopStagesCorrelateUnderOneRequestID(t *testing.T) {
	// The session drops frames captured before it starts, so stage the question
	// once it is recording instead of scripting loud frames that a slow
	// scheduler could lose entirely (#332).
	source := &stagedSource{}
	wake := &fakeWake{afterWords: 5}
	stt := &fakeSTT{text: "what time is it"}
	recording := make(chan struct{})
	var once sync.Once
	stt.onWrite = func(stream int) {
		if stream == 1 {
			once.Do(func() { close(recording) })
		}
	}
	var recorded atomic.Value // *latency.Timeline
	responder := &fakeResponder{
		calls: make(chan string, 1),
		onCall: func(ctx context.Context) {
			recorded.Store(latency.FromContext(ctx))
		},
	}
	logs := &lockedBuffer{}
	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint:   endpointConfig(500 * time.Millisecond),
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Responder: responder,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	})
	runLoop(t, loop)

	waitForSignal(t, recording, "the session never started recording audio")
	source.stage(utteranceFrames())
	waitForReply(t, responder.calls, "no reply")

	value := recorded.Load()
	if value == nil {
		t.Fatal("responder context carried no request timeline")
	}
	timeline := value.(*latency.Timeline)
	if timeline.ID() == "" {
		t.Fatal("timeline has no request ID")
	}
	// The fake source delivers frames faster than real time, so this only
	// checks the stage is measured; the exact last-voiced-frame rule is a
	// unit test on the endpointer's clock.
	if _, ok := timeline.Duration(latency.StageEndpointing); !ok {
		t.Fatal("endpointing stage missing")
	}
	if _, ok := timeline.Duration(latency.StageSTTFinalize); !ok {
		t.Fatal("stt_finalize stage missing")
	}
	// The stage is a measurement only if the question reached the session.
	var voiced bool
	for _, frame := range stt.received() {
		if !isSilent(frame) {
			voiced = true
			break
		}
	}
	if !voiced {
		t.Fatal("the session never recorded a voiced frame")
	}

	// The endpoint and transcript lines carry the same request ID.
	var seenEndpoint, seenTranscript bool
	for _, line := range strings.Split(logs.String(), "\n") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		if parsed["request_id"] != timeline.ID() {
			continue
		}
		switch parsed["msg"] {
		case "voice: endpoint":
			seenEndpoint = true
		case "voice: transcript":
			seenTranscript = true
		}
	}
	if !seenEndpoint || !seenTranscript {
		t.Fatalf("voice lines not correlated with %q (endpoint=%v transcript=%v):\n%s",
			timeline.ID(), seenEndpoint, seenTranscript, logs.String())
	}
}

func TestLoopDropsBufferedChimeEchoFrames(t *testing.T) {
	// Loud frames are captured while the chime plays and sit in the frame
	// buffer. Once chimeDone fires they must be dropped, not endpointed, or the
	// chime tail opens a false utterance (#18/S6).
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 100)...)}
	wake := &fakeWake{afterWords: 3}
	stt := &fakeSTT{text: "hello"}
	player := &fakePlayer{block: 500 * time.Millisecond}
	responder := &fakeResponder{calls: make(chan string, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint: voice.EndpointConfig{
			Rate:          16000,
			Silence:       30 * time.Millisecond,
			ThresholdDBFS: -38,
			// No cap: the scripted source runs much faster than real time, and
			// a cap would end the turn even without speech.
			MaxUtterance: 0,
		},
		Chime:       []byte{1, 2, 3, 4},
		ChimeFormat: audio.Format{Rate: 16000, Width: 2, Channels: 1},
	}, voice.Deps{Source: source, Wake: wake, STT: stt, Player: player, Responder: responder})
	cancel, _ := runLoop(t, loop)

	select {
	case got := <-responder.calls:
		t.Fatalf("responded %q on buffered chime echo, want no utterance", got)
	case <-time.After(1200 * time.Millisecond):
	}
	cancel()

	for _, frame := range stt.received() {
		if !isSilent(frame) {
			t.Fatal("a chime echo frame reached STT; only post-chime audio should")
		}
	}
}

// TestLoopFeedsTheActivityObserver proves the voice path hands its completed
// request to the same store the Dashboard reads. The responder plays the part
// the reply session does in production: set route and reply, then complete the
// one timeline.
func TestLoopFeedsTheActivityObserver(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5}
	stt := &fakeSTT{text: "what time is it"}
	completed := make(chan latency.Completed, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := &fakeResponder{
		calls: make(chan string, 1),
		onCall: func(ctx context.Context) {
			latency.SetRoute(ctx, "control_phrase", "time.now", "what time is it")
			latency.SetSummary(ctx, "It's 3:04 PM.", false, false)
			latency.Complete(ctx, logger, latency.OutcomeNoAudio, nil)
		},
	}
	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint:   endpointConfig(500 * time.Millisecond),
	}, voice.Deps{
		Source: source, Wake: wake, STT: stt, Responder: responder,
		Observer: func(c latency.Completed) {
			select {
			case completed <- c:
			default:
			}
		},
		Logger: logger,
	})
	runLoop(t, loop)

	select {
	case got := <-completed:
		if got.Source != latency.SourceVoice || got.Path != "control_phrase" || got.Tool != "time.now" {
			t.Fatalf("completed = %+v", got)
		}
		if got.Transcript != "what time is it" || got.Detail == nil || got.Detail.Reply != "It's 3:04 PM." {
			t.Fatalf("completed detail = %+v", got.Detail)
		}
		if got.HasResponse {
			t.Fatal("a text-only fake reply claimed a Response latency")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("voice completion did not reach the observer")
	}
}

// followUpFixture runs one no-wake loop and exposes the transitions its cases
// synchronize on, so no case depends on how fast the scheduler drains frames.
type followUpFixture struct {
	source    *stagedSource
	wake      *fakeWake
	stt       *fakeSTT
	responder *fakeResponder
	// capturing closes when the no-wake session records its first frame: its
	// speaker-tail gate is open and the window is listening.
	capturing chan struct{}
	// wakeWaiting closes when the loop is back in waitForWake after the first
	// reply, which is the state the disabled and expired cases refuse speech in.
	wakeWaiting chan struct{}
}

func newFollowUpFixture(t *testing.T, window time.Duration) *followUpFixture {
	t.Helper()
	fixture := &followUpFixture{
		source:      &stagedSource{script: utteranceFrames()},
		capturing:   make(chan struct{}),
		wakeWaiting: make(chan struct{}),
	}
	var capturing, waiting sync.Once
	fixture.stt = &fakeSTT{text: "what time is it", onWrite: func(stream int) {
		if stream == 2 {
			capturing.Do(func() { close(fixture.capturing) })
		}
	}}
	// Streams open in order: the first detection, the reply's barge-in watch,
	// then either the no-wake session's watch or a fresh detection.
	fixture.wake = &fakeWake{afterWords: 3, maxFires: 1, onOpen: func(stream int) {
		if stream >= 3 {
			waiting.Do(func() { close(fixture.wakeWaiting) })
		}
	}}
	fixture.responder = &fakeResponder{calls: make(chan string, 4)}
	loop := newLoop(t, voice.Config{FrameBytes: frameBytes, Endpoint: endpointConfig(300 * time.Millisecond)},
		voice.Deps{Source: fixture.source, Wake: fixture.wake, STT: fixture.stt, Responder: fixture.responder})
	// Only the first reply opens a window, so the loop settles after the
	// transition each case is about.
	var replies atomic.Int32
	loop.BindFollowUp(func() time.Duration {
		if replies.Add(1) > 1 {
			return 0
		}
		return window
	})
	runLoop(t, loop)
	return fixture
}

// waitForLoudFrames waits until staged speech reaches the wake service, which
// proves the loop consumed it instead of dropping it at a closed window.
func (f *followUpFixture) waitForLoudFrames(t *testing.T) {
	t.Helper()
	before := f.wake.loudFrames()
	deadline := time.Now().Add(5 * time.Second)
	for f.wake.loudFrames() == before {
		if time.Now().After(deadline) {
			t.Fatal("the staged utterance never reached the wake service")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A completed reply opens a no-wake turn, while a zero window and an expired
// window leave the wake gate closed. The second utterance is staged at a real
// loop transition, never padded with silence: silence padding ties the fixture
// to scheduler pace and expired under load (#332).
func TestFollowUpWithoutWakeAndDisable(t *testing.T) {
	tests := []struct {
		name   string
		window time.Duration
		// accepts reports whether the second utterance should start a turn.
		accepts bool
		// wantSTTOpens counts capture sessions: the first reply, plus the
		// no-wake session when a live window opens.
		wantSTTOpens int
	}{
		{name: "accepts", window: time.Second, accepts: true, wantSTTOpens: 2},
		{name: "disabled", window: 0, wantSTTOpens: 1},
		// Shorter than the first reply's window: the no-wake session must expire
		// before its utterance cap turns the silence into a request.
		{name: "expired", window: 500 * time.Millisecond, wantSTTOpens: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFollowUpFixture(t, test.window)
			waitForReply(t, fixture.responder.calls, "first turn missing")

			if test.accepts {
				waitForSignal(t, fixture.capturing, "the no-wake window never captured audio")
			} else {
				waitForSignal(t, fixture.wakeWaiting, "the loop never went back to waiting for a wake word")
			}
			if got := fixture.stt.openCount(); got != test.wantSTTOpens {
				t.Fatalf("STT opens before the second utterance = %d, want %d", got, test.wantSTTOpens)
			}

			fixture.source.stage(utteranceFrames())
			if test.accepts {
				transcript := waitForReply(t, fixture.responder.calls, "no-wake follow-up missing")
				if transcript != "what time is it" {
					t.Fatalf("follow-up transcript = %q", transcript)
				}
				words := fixture.responder.wakeWords()
				if len(words) != 2 || words[1] != "" {
					t.Fatalf("follow-up wake provenance = %v, want the second turn with no wake word", words)
				}
				return
			}

			// Refusal only counts once the staged speech reached the wake
			// service: the loop was listening for a wake word and heard it.
			fixture.waitForLoudFrames(t)
			select {
			case got := <-fixture.responder.calls:
				t.Fatalf("%v window accepted a no-wake utterance %q", test.window, got)
			case <-time.After(time.Second):
			}
			if got := fixture.stt.openCount(); got != test.wantSTTOpens {
				t.Fatalf("STT opens after the staged speech = %d, want %d", got, test.wantSTTOpens)
			}
		})
	}
}
