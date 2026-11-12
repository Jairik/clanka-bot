package speech

import (
	"context"
	"io"
	"sync"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// SerialSink lets one writer play at a time. The assistant is single-user and
// has one speaker, so a reply, a Filler or a timer alert must never overlap.
//
// Serialisation starts at the first Write, not Open: TTS opens its sink before
// it has any audio, and a Filler must be able to play during that synthesis
// gap. Whichever writer submits the first PCM holds the gate until Close (when
// pacat has drained), so the reply either goes first and suppresses the Filler
// or waits for a Filler already playing.
type SerialSink struct {
	inner Sink
	gate  chan struct{}
}

// NewSerialSink wraps a sink. A nil sink stays nil so callers can keep the
// "speech is optional" behaviour.
func NewSerialSink(inner Sink) *SerialSink {
	if inner == nil {
		return nil
	}
	return &SerialSink{inner: inner, gate: make(chan struct{}, 1)}
}

// Open opens the underlying sink. The returned writer acquires the gate on its
// first Write.
func (s *SerialSink) Open(ctx context.Context, format audio.Format) (io.WriteCloser, error) {
	writer, err := s.inner.Open(ctx, format)
	if err != nil {
		return nil, err
	}
	return &serialWriter{ctx: ctx, inner: writer, gate: s.gate}, nil
}

type serialWriter struct {
	ctx   context.Context
	inner io.WriteCloser
	gate  chan struct{}

	// acquireMu makes the first Write the only one that acquires the gate and
	// records it; Close reads the same fields under it.
	acquireMu sync.Mutex
	acquired  bool

	mu     sync.Mutex
	closed bool
}

func (w *serialWriter) Write(p []byte) (int, error) {
	if err := w.acquireGate(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return 0, context.Canceled
	}
	return w.inner.Write(p)
}

func (w *serialWriter) acquireGate() error {
	w.acquireMu.Lock()
	defer w.acquireMu.Unlock()
	if w.acquired {
		return nil
	}
	select {
	case w.gate <- struct{}{}:
		w.acquired = true
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func (w *serialWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	err := w.inner.Close()

	w.acquireMu.Lock()
	acquired := w.acquired
	w.acquired = false
	w.acquireMu.Unlock()
	if acquired {
		<-w.gate
	}
	return err
}
