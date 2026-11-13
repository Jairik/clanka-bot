package voice

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
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
