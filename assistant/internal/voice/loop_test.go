package voice_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
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

// fakeWake fires once per opened stream after the configured number of frames.
type fakeWake struct {
	afterWords int

	mu          sync.Mutex
	streams     int
	openStreams int
	fires       int
}

func (f *fakeWake) Open(ctx context.Context) (voice.WakeStream, error) {
	f.mu.Lock()
	f.streams++
	f.openStreams++
	f.mu.Unlock()
	return &fakeWakeStream{owner: f, detections: make(chan voice.Detection, 1)}, nil
}

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

	if frames == s.owner.afterWords {
		s.owner.mu.Lock()
		s.owner.fires++
		s.owner.mu.Unlock()
		select {
		case s.detections <- voice.Detection{Name: "okay_nabu"}:
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

	mu      sync.Mutex
	frames  [][]byte
	opens   int
	finish  int
	closing bool
}

func (f *fakeSTT) Open(ctx context.Context) (voice.STTStream, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return &fakeSTTStream{owner: f}, nil
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

type fakeSTTStream struct{ owner *fakeSTT }

func (s *fakeSTTStream) Write(pcm []byte) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.owner.frames = append(s.owner.frames, append([]byte(nil), pcm...))
	return nil
}

func (s *fakeSTTStream) Finish() (string, error) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.owner.finish++
	return s.owner.text, nil
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
// the reply started: the wake stream must already be closed (muted).
type fakeResponder struct {
	owner  *ownerRef
	block  time.Duration
	calls  chan string
	onCall func(ctx context.Context)
}

type ownerRef struct {
	wake        *fakeWake
	openAtReply atomic.Int32
}

func (r *fakeResponder) Respond(ctx context.Context, transcript string) error {
	if r.owner != nil {
		_, open, _ := r.owner.wake.snapshot()
		r.owner.openAtReply.Store(int32(open))
	}
	if r.onCall != nil {
		r.onCall(ctx)
	}
	if r.block > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.block):
		}
	}
	select {
	case r.calls <- transcript:
	default:
	}
	return nil
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

func TestLoopChimesOnceMutesWakeAndAnswers(t *testing.T) {
	source := &scriptSource{frames: append(repeat(loudFrame(), 8), repeat(silentFrame(), 40)...)}
	wake := &fakeWake{afterWords: 5}
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
	if open := owner.openAtReply.Load(); open != 0 {
		t.Fatalf("wake streams open at reply = %d, want 0 (muted)", open)
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
