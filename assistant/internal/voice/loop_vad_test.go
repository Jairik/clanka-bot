package voice_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

type stubVADService struct {
	mu      sync.Mutex
	opens   int
	openErr error
	scores  []float64
	closed  int
}

func (s *stubVADService) Open(context.Context) (voice.VADSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if s.openErr != nil {
		return nil, s.openErr
	}
	return &stubVADSession{parent: s, scores: append([]float64(nil), s.scores...)}, nil
}

type stubVADSession struct {
	parent *stubVADService
	scores []float64
	idx    int
}

func (s *stubVADSession) Score([]byte) (float64, error) {
	if s.idx < len(s.scores) {
		v := s.scores[s.idx]
		s.idx++
		return v, nil
	}
	return 0, nil
}

func (s *stubVADSession) Close() error {
	s.parent.mu.Lock()
	s.parent.closed++
	s.parent.mu.Unlock()
	return nil
}

type vadTestResponder struct {
	got chan voice.Request
}

func (r *vadTestResponder) Respond(_ context.Context, req voice.Request) error {
	select {
	case r.got <- req:
	default:
	}
	return nil
}

func TestLoopUsesSileroVADAndClosesSession(t *testing.T) {
	// Every frame is loud (well above -38 dBFS RMS), so RMS alone would never
	// end the turn. Silero VAD rejects the first 2 room-noise frames, opens on
	// the next 3 speech frames, then ends after 5 non-speech frames (160 ms).
	pcm := make([][]byte, 14)
	for i := range pcm {
		pcm[i] = loudFrame()
	}
	src := &scriptSource{frames: pcm}
	wake := &fakeWake{afterWords: 1, name: "clanker", maxFires: 1}
	stt := &fakeSTT{text: "what time is it"}
	vad := &stubVADService{
		scores: []float64{0.05, 0.05, 0.9, 0.9, 0.9, 0.05, 0.05, 0.05, 0.05, 0.05},
	}
	resp := &vadTestResponder{got: make(chan voice.Request, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint: voice.EndpointConfig{
			Rate:          16000,
			VAD:           true,
			VADThreshold:  0.45,
			Silence:       160 * time.Millisecond,
			ThresholdDBFS: -38,
			MinSpeech:     64 * time.Millisecond,
			MaxUtterance:  5 * time.Second,
		},
	}, voice.Deps{
		Source:    src,
		Wake:      wake,
		STT:       stt,
		VAD:       vad,
		Responder: resp,
		WakeWords: []string{"clanker"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	select {
	case req := <-resp.got:
		if req.Transcript != "what time is it" {
			t.Fatalf("transcript = %q", req.Transcript)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for VAD-gated turn")
	}
	cancel()
	<-done

	vad.mu.Lock()
	opens, closed := vad.opens, vad.closed
	vad.mu.Unlock()
	if opens != 1 || closed != 1 {
		t.Fatalf("vad opens=%d closed=%d, want 1 and 1", opens, closed)
	}
}

func TestLoopFallsBackToRMSWhenVADOpenFails(t *testing.T) {
	var pcm [][]byte
	pcm = append(pcm, silentFrame()) // wake fires on frame 1
	for i := 0; i < 4; i++ {
		pcm = append(pcm, loudFrame()) // speech for RMS
	}
	for i := 0; i < 6; i++ {
		pcm = append(pcm, silentFrame()) // silence ends turn
	}
	src := &scriptSource{frames: pcm}
	wake := &fakeWake{afterWords: 1, name: "clanker", maxFires: 1}
	stt := &fakeSTT{text: "turn on the lights"}
	vad := &stubVADService{openErr: errors.New("vad-unavailable")}
	resp := &vadTestResponder{got: make(chan voice.Request, 1)}

	loop := newLoop(t, voice.Config{
		FrameBytes: frameBytes,
		Endpoint: voice.EndpointConfig{
			Rate:          16000,
			VAD:           true,
			VADThreshold:  0.45,
			Silence:       160 * time.Millisecond,
			ThresholdDBFS: -38,
			MinSpeech:     64 * time.Millisecond,
			MaxUtterance:  5 * time.Second,
		},
	}, voice.Deps{
		Source:    src,
		Wake:      wake,
		STT:       stt,
		VAD:       vad,
		Responder: resp,
		WakeWords: []string{"clanker"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	select {
	case req := <-resp.got:
		if req.Transcript != "turn on the lights" {
			t.Fatalf("transcript = %q", req.Transcript)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RMS fallback turn")
	}
	cancel()
	<-done
}
