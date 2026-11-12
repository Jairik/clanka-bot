package speech_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

// blockingSink records when each writer actually gets to write.
type blockingSink struct {
	mu        sync.Mutex
	events    []string
	first     chan struct{}
	firstOnce sync.Once
	release   chan struct{}
}

func newBlockingSink() *blockingSink {
	return &blockingSink{first: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return &blockingWriter{sink: s}, nil
}

type blockingWriter struct {
	sink *blockingSink
	once sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		w.sink.mu.Lock()
		w.sink.events = append(w.sink.events, "write")
		w.sink.mu.Unlock()
		w.sink.firstOnce.Do(func() { close(w.sink.first) })
		<-w.sink.release
	})
	return len(p), nil
}

func (w *blockingWriter) Close() error { return nil }

func TestSerialSinkLetsOnlyOneWriterPlayAtATime(t *testing.T) {
	inner := newBlockingSink()
	sink := speech.NewSerialSink(inner)
	ctx := context.Background()
	format := audio.Format{Rate: 24000, Width: 2, Channels: 1}

	first, err := sink.Open(ctx, format)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	second, err := sink.Open(ctx, format)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = first.Write([]byte{1, 2})
		_ = first.Close()
		close(done)
	}()
	select {
	case <-inner.first:
	case <-time.After(time.Second):
		t.Fatal("first writer never wrote")
	}

	wroteSecond := make(chan struct{})
	go func() {
		_, _ = second.Write([]byte{3, 4})
		_ = second.Close()
		close(wroteSecond)
	}()
	select {
	case <-wroteSecond:
		t.Fatal("second writer wrote while the first still held the gate")
	case <-time.After(50 * time.Millisecond):
	}

	close(inner.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("first writer did not finish")
	}
	select {
	case <-wroteSecond:
	case <-time.After(time.Second):
		t.Fatal("second writer never got the gate")
	}

	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.events) != 2 {
		t.Fatalf("events = %v, want two serialized writes", inner.events)
	}
}
