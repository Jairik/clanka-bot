package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// Sample capture must be bounded, exclusive and released on cancellation.
func TestRecordSample(t *testing.T) {
	l := &Loop{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []byte, 1)
	go func() { pcm, _ := l.RecordSample(ctx); done <- pcm }()
	deadline := time.After(time.Second)
	var frames chan []byte
	for frames == nil {
		l.sampleMu.Lock()
		frames = l.sampleFrames
		l.sampleMu.Unlock()
		select {
		case <-deadline:
			t.Fatal("capture did not attach")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := l.RecordSample(ctx); err == nil {
		t.Fatal("concurrent take accepted")
	}
	frames <- bytes.Repeat([]byte{1}, 32000)
	frames <- bytes.Repeat([]byte{2}, 32000)
	frames <- bytes.Repeat([]byte{3}, 32000)
	select {
	case pcm := <-done:
		if len(pcm) != 96000 || pcm[0] != 1 || pcm[95999] != 3 {
			t.Fatal("incorrect captured PCM")
		}
	case <-time.After(time.Second):
		t.Fatal("capture hung")
	}
	l.sampleMu.Lock()
	active := l.sampling
	l.sampleMu.Unlock()
	if active {
		t.Fatal("sample gate remained active")
	}
	l.inTurn = true
	if _, err := l.RecordSample(ctx); err == nil {
		t.Fatal("recording interrupted a voice turn")
	}
	l.inTurn = false
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if _, err := l.RecordSample(stopped); err == nil {
		t.Fatal("cancelled recording succeeded")
	}
	if l.sampling {
		t.Fatal("cancelled recording retained gate")
	}
}

// The normal voice path must never receive the owner's sample PCM.
func TestSampleTapSilencesNormalFrames(t *testing.T) {
	raw := bytes.Repeat([]byte{42}, 320)
	l := &Loop{cfg: Config{FrameBytes: 320}, sampling: true, sampleFrames: make(chan []byte, 1)}
	l.deps.Source = fixedSampleSource{raw}
	frames := make(chan micFrame, 1)
	_ = l.captureOnce(context.Background(), frames)
	if got := <-l.sampleFrames; !bytes.Equal(got, raw) {
		t.Fatal("tap did not preserve original PCM")
	}
	if got := <-frames; !bytes.Equal(got.pcm, make([]byte, 320)) {
		t.Fatal("sample speech reached normal voice path")
	}
}

// A finite source makes the capture privacy test deterministic without audio hardware.
type fixedSampleSource struct{ pcm []byte }

func (s fixedSampleSource) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.pcm)), nil
}

// countingPlayer records how many cue buffers it played.
type countingPlayer struct {
	mu    sync.Mutex
	calls int
}

func (p *countingPlayer) Play(context.Context, []byte, audio.Format) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return nil
}

func (p *countingPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// RecordSample must skip its 880 Hz cue while a Ring owns the floor: capture
// starts immediately and the player stays silent. Outside a Ring the cue plays.
func TestRecordSampleSkipsCueDuringRing(t *testing.T) {
	for _, ring := range []bool{true, false} {
		t.Run(fmt.Sprintf("ringActive=%v", ring), func(t *testing.T) {
			player := &countingPlayer{}
			active := ring
			l := &Loop{
				cfg: Config{FrameBytes: 320},
				deps: Deps{
					Player:     player,
					RingActive: func() bool { return active },
					Logger:     slog.Default(),
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan ([]byte), 1)
			go func() { pcm, _ := l.RecordSample(ctx); done <- pcm }()

			var frames chan []byte
			deadline := time.After(time.Second)
			for frames == nil {
				l.sampleMu.Lock()
				frames = l.sampleFrames
				l.sampleMu.Unlock()
				select {
				case <-deadline:
					t.Fatal("capture did not attach")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			frames <- bytes.Repeat([]byte{1}, 32000)
			frames <- bytes.Repeat([]byte{2}, 32000)
			frames <- bytes.Repeat([]byte{3}, 32000)
			select {
			case pcm := <-done:
				if len(pcm) != 96000 {
					t.Fatalf("captured %d bytes, want 96000", len(pcm))
				}
			case <-time.After(2 * time.Second):
				t.Fatal("capture hung")
			}
			if got := player.count(); ring && got != 0 {
				t.Fatalf("cue played %d times during a Ring, want none", got)
			}
			if got := player.count(); !ring && got != 1 {
				t.Fatalf("cue played %d times outside a Ring, want once", got)
			}
		})
	}
}
