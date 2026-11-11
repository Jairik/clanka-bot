package speech_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// recorder is a Sink that remembers every chunk and signals the first write.
type recorder struct {
	mu      sync.Mutex
	chunks  [][]byte
	closed  bool
	opened  audio.Format
	firstCh chan struct{}
	once    sync.Once
}

func newRecorder() *recorder {
	return &recorder{firstCh: make(chan struct{})}
}

func (r *recorder) Open(_ context.Context, format audio.Format) (io.WriteCloser, error) {
	r.mu.Lock()
	r.opened = format
	r.mu.Unlock()
	return r, nil
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.chunks = append(r.chunks, append([]byte(nil), p...))
	r.mu.Unlock()
	r.once.Do(func() { close(r.firstCh) })
	return len(p), nil
}

func (r *recorder) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

func (r *recorder) audio() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []byte
	for _, chunk := range r.chunks {
		out = append(out, chunk...)
	}
	return out
}

func writeEvent(t *testing.T, w io.Writer, typ string, data any) {
	t.Helper()
	ev := map[string]any{"type": typ}
	if data != nil {
		ev["data"] = data
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func writeAudio(t *testing.T, w io.Writer, pcm []byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": "audio-chunk", "payload_length": len(pcm)})
	if err != nil {
		t.Fatalf("marshal audio: %v", err)
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if _, err := w.Write(pcm); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

func TestSayPlaysTheFirstChunkBeforeSynthesisFinishes(t *testing.T) {
	firstPlayed := make(chan struct{})
	chunkOne := bytes.Repeat([]byte{1, 2}, 12000) // 0.5 s at 24 kHz, s16 mono
	chunkTwo := bytes.Repeat([]byte{3, 4}, 12000)
	addr := serveOnce(t, func(conn net.Conn) {
		readLine(t, conn)
		writeEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, conn, chunkOne)
		// Only finish synthesising once playback has started.
		select {
		case <-firstPlayed:
		case <-time.After(2 * time.Second):
			t.Error("playback did not start before synthesis continued")
			return
		}
		writeAudio(t, conn, chunkTwo)
		writeEvent(t, conn, "audio-stop", nil)
	})

	sink := newRecorder()
	client := wyoming.NewClient(addr)
	speaker := speech.New(client, sink, wyoming.Voice{Name: "alba"}, nil)
	go func() {
		<-sink.firstCh
		close(firstPlayed)
	}()

	result, err := speaker.Say(context.Background(), "Hello there.")
	if err != nil {
		t.Fatalf("Say: %v", err)
	}
	if got := sink.audio(); !bytes.Equal(got, append(append([]byte(nil), chunkOne...), chunkTwo...)) {
		t.Fatalf("played %d bytes, want %d", len(got), len(chunkOne)+len(chunkTwo))
	}
	if sink.opened != (audio.Format{Rate: 24000, Width: 2, Channels: 1}) {
		t.Fatalf("sink format = %+v", sink.opened)
	}
	if result.AudioBytes != 48000 || result.Chunks != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.AudioTime != time.Second {
		t.Fatalf("audio time = %s, want 1s", result.AudioTime)
	}
	if !sink.closed {
		t.Fatal("sink was not closed")
	}
}

func TestSayReturnsErrorWhenSpeechServiceFails(t *testing.T) {
	addr := serveOnce(t, func(conn net.Conn) {
		readLine(t, conn)
		writeEvent(t, conn, "error", map[string]any{"text": "model failed"})
	})

	sink := newRecorder()
	speaker := speech.New(wyoming.NewClient(addr), sink, wyoming.Voice{}, nil)
	if _, err := speaker.Say(context.Background(), "Hello."); err == nil {
		t.Fatal("Say succeeded, want error")
	}
	if len(sink.chunks) != 0 {
		t.Fatalf("sink received %d chunks, want none", len(sink.chunks))
	}
}

func serveOnce(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}()
	return ln.Addr().String()
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Errorf("read request: %v", err)
	}
	return line
}
