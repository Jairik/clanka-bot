package speech_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestSerialSinkReleasesTheGateAfterAStalledClose is the #203 regression: the
// first stream's pacat stops draining, so its Close is bounded instead of
// waiting forever. The gate must open at that bound and let the next sound
// play; otherwise every later reply, Filler, Cue, Ring and Announcement blocks
// in acquireGate and the assistant is silent until a restart.
func TestSerialSinkReleasesTheGateAfterAStalledClose(t *testing.T) {
	dir := t.TempDir()
	secondPCM := filepath.Join(dir, "second.raw")
	pidPath := filepath.Join(dir, "pid")
	binary := filepath.Join(dir, "pacat")
	// The stream opened for the "stall" device ignores stdin and never exits,
	// which is what a suspended sink leaves behind. The next one drains and
	// exits like a healthy pacat.
	script := `#!/bin/sh
case "$*" in
*--device=stall*) echo $$ > '` + pidPath + `'; exec sleep 30 ;;
esac
cat > '` + secondPCM + `'
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	t.Cleanup(func() {
		if text, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(text))); err == nil {
				if process, err := os.FindProcess(pid); err == nil {
					_ = process.Kill()
				}
			}
		}
	})

	sink := audio.NewPulseSink(binary, "")
	// The first Open takes the stalled device; every later Open a healthy one.
	var openMu sync.Mutex
	opens := 0
	sink.Device = func() string {
		openMu.Lock()
		defer openMu.Unlock()
		opens++
		if opens == 1 {
			return "stall"
		}
		return "healthy"
	}
	serial := speech.NewSerialSink(sink)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	format := audio.Format{Rate: 24000, Width: 2, Channels: 1}

	first, err := serial.Open(ctx, format)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	second, err := serial.Open(ctx, format)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}

	// The first Write returns only after it holds the gate, so the second
	// writer is guaranteed to queue behind a stream that is already stuck.
	firstWrote := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		if _, err := first.Write([]byte{1, 2}); err != nil {
			firstDone <- err
			return
		}
		close(firstWrote)
		firstDone <- first.Close()
	}()
	select {
	case <-firstWrote:
	case err := <-firstDone:
		t.Fatalf("first writer: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first writer never wrote")
	}

	secondDone := make(chan error, 1)
	go func() {
		if _, err := second.Write([]byte{3, 4}); err != nil {
			secondDone <- err
			return
		}
		secondDone <- second.Close()
	}()

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second writer: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("second writer never got the gate: the stalled Close still holds it")
	}

	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("stalled Close = nil, want a drain-stall error")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("first writer did not finish")
	}

	pcm, err := os.ReadFile(secondPCM)
	if err != nil {
		t.Fatalf("read second pcm: %v", err)
	}
	if string(pcm) != string([]byte{3, 4}) {
		t.Fatalf("second pcm = %v, want [3 4]", pcm)
	}
}
