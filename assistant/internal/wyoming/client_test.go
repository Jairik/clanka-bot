package wyoming_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// fakeServer is a one-connection Wyoming server for tests. handle runs with the
// accepted connection before the client is used.
func fakeServer(t *testing.T, handle func(conn net.Conn)) string {
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

type wireEvent struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
	Version string          `json:"version"`
}

func readWireEvent(t *testing.T, r *bufio.Reader) wireEvent {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Errorf("read event: %v", err)
		return wireEvent{}
	}
	var ev wireEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Errorf("decode event %q: %v", line, err)
	}
	return ev
}

func writeWireEvent(t *testing.T, w io.Writer, typ string, data any) {
	t.Helper()
	ev := map[string]any{"type": typ}
	if data != nil {
		ev["data"] = data
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Errorf("marshal event: %v", err)
		return
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		t.Errorf("write event: %v", err)
	}
}

func writeAudio(t *testing.T, w io.Writer, typ string, pcm []byte) {
	t.Helper()
	header := map[string]any{"type": typ, "payload_length": len(pcm)}
	raw, err := json.Marshal(header)
	if err != nil {
		t.Errorf("marshal audio event: %v", err)
		return
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		t.Errorf("write audio header: %v", err)
		return
	}
	if _, err := w.Write(pcm); err != nil {
		t.Errorf("write audio payload: %v", err)
	}
}

func dataMap(t *testing.T, ev wireEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Errorf("decode data: %v", err)
		}
	}
	return data
}

func TestSynthesizeStreamsAudioChunks(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		req := readWireEvent(t, r)
		if req.Type != "synthesize" {
			t.Errorf("request type = %q, want synthesize", req.Type)
		}
		data := dataMap(t, req)
		if got := data["text"]; got != "Hello there." {
			t.Errorf("text = %v", got)
		}
		voice, _ := data["voice"].(map[string]any)
		if got := voice["name"]; got != "alba" {
			t.Errorf("voice name = %v", got)
		}

		writeWireEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, conn, "audio-chunk", []byte{1, 2})
		writeAudio(t, conn, "audio-chunk", []byte{3, 4})
		writeWireEvent(t, conn, "audio-stop", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.Synthesize(ctx, "Hello there.", wyoming.Voice{Name: "alba"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()
	if stream.Format != (wyoming.Format{Rate: 24000, Width: 2, Channels: 1}) {
		t.Fatalf("format = %+v", stream.Format)
	}

	var got []byte
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, chunk...)
	}
	if string(got) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("audio = %v, want [1 2 3 4]", got)
	}
}

func TestSynthesizeReportsServerError(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		readWireEvent(t, bufio.NewReader(conn))
		writeWireEvent(t, conn, "error", map[string]any{"text": "model is on fire", "code": "synthesis-failed"})
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	_, err := client.Synthesize(ctx, "hi", wyoming.Voice{})
	if err == nil || !strings.Contains(err.Error(), "model is on fire") {
		t.Fatalf("error = %v, want server text", err)
	}
}

func TestDescribeReadsTtsInfo(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		req := readWireEvent(t, bufio.NewReader(conn))
		if req.Type != "describe" {
			t.Errorf("request type = %q, want describe", req.Type)
		}
		writeWireEvent(t, conn, "info", map[string]any{
			"tts": []map[string]any{{
				"name":        "pocket-tts",
				"description": "Pocket TTS",
				"installed":   true,
				"version":     "4.0.1",
				"languages":   []string{"en"},
				"voices": []map[string]any{{
					"name":      "alba",
					"languages": []string{"en"},
				}},
				"supports_synthesize_streaming": true,
			}},
		})
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	info, err := client.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(info.TTS) != 1 {
		t.Fatalf("tts programs = %d, want 1", len(info.TTS))
	}
	program := info.TTS[0]
	if program.Name != "pocket-tts" || !program.SupportsSynthesizeStreaming {
		t.Fatalf("program = %+v", program)
	}
	if len(program.Voices) != 1 || program.Voices[0].Name != "alba" {
		t.Fatalf("voices = %+v", program.Voices)
	}
}

func TestOpenStreamSendsTextChunksAndReadsSegments(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		start := readWireEvent(t, r)
		if start.Type != "synthesize-start" {
			t.Errorf("first event = %q, want synthesize-start", start.Type)
		}
		chunk := readWireEvent(t, r)
		chunkData := dataMap(t, chunk)
		if chunk.Type != "synthesize-chunk" || chunkData["text"] != "One." {
			t.Errorf("chunk = %q data=%v", chunk.Type, chunkData)
		}
		stop := readWireEvent(t, r)
		if stop.Type != "synthesize-stop" {
			t.Errorf("stop = %q", stop.Type)
		}

		for _, sample := range [][]byte{{10}, {20}} {
			writeWireEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
			writeAudio(t, conn, "audio-chunk", sample)
			writeWireEvent(t, conn, "audio-stop", nil)
		}
		writeWireEvent(t, conn, "synthesize-stopped", nil)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	defer cancel()

	stream, err := client.OpenStream(ctx, wyoming.Voice{Name: "alba"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()
	if err := stream.SendText("One."); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := stream.EndText(); err != nil {
		t.Fatalf("EndText: %v", err)
	}

	var got []byte
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, chunk...)
	}
	if string(got) != string([]byte{10, 20}) {
		t.Fatalf("audio = %v, want [10 20]", got)
	}
}

func TestSynthesizeHonoursContextCancellation(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		readWireEvent(t, bufio.NewReader(conn))
		writeWireEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeAudio(t, conn, "audio-chunk", []byte{1, 2})
		// Stall until the client goes away.
		_, _ = io.Copy(io.Discard, conn)
	})

	client := wyoming.NewClient(addr)
	ctx, cancel := contextWithTimeout(t)
	stream, err := client.Synthesize(ctx, "hi", wyoming.Voice{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	cancel()
	if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next after cancel = %v, want context.Canceled", err)
	}
}

func contextWithTimeout(t *testing.T) (ctx context.Context, cancel context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}
