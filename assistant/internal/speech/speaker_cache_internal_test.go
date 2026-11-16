package speech

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// memSink discards everything written to it; it is only used to drive Say
// without touching the real audio device.
type memSink struct{}

func (m *memSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return memWriter{}, nil
}

type memWriter struct{}

func (m memWriter) Write(p []byte) (int, error) { return len(p), nil }
func (m memWriter) Close() error                { return nil }

func writeWyomingEvent(t *testing.T, w io.Writer, typ string, data map[string]any) {
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

func writeWyomingAudio(t *testing.T, w io.Writer, pcm []byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": "audio-chunk", "payload_length": len(pcm)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write(pcm); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

// TestTTSCacheDoesNotBufferLongReplies verifies that a reply longer than the
// rune cap is never buffered into the cache, so its whole PCM is not held in
// memory — the exact regression the 1 MiB cap exists to prevent on the Pi.
func TestTTSCacheDoesNotBufferLongReplies(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				bufio.NewReader(c).ReadString('\n') // the synthesize request
				writeWyomingEvent(t, c, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
				// Several large chunks of a long reply.
				for i := 0; i < 5; i++ {
					writeWyomingAudio(t, c, bytes.Repeat([]byte{1, 2}, 40000)) // 80 KB each
				}
				writeWyomingEvent(t, c, "audio-stop", nil)
			}(conn)
		}
	}()

	sp := New(wyoming.NewClient(ln.Addr().String()), &memSink{}, wyoming.Voice{Name: "alba"}, nil)
	long := strings.Repeat("a", 200) // 200 runes, over the 80-rune cap.
	if _, err := sp.Say(context.Background(), long); err != nil {
		t.Fatalf("Say: %v", err)
	}
	// The cache must remain empty: no entry was created and no PCM accumulated.
	if sp.ttsCache != nil {
		t.Fatalf("long reply populated the cache with %d bytes; it should stay empty", sp.ttsCache.bytes)
	}
}
