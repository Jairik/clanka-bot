package voicesample_test

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// startSampleServer answers one synthesize request with the supplied frames.
func startSampleServer(t *testing.T, format map[string]any, chunks [][]byte, fail bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(line, &request) != nil || request.Type != "synthesize" {
			return
		}
		write := func(value map[string]any, payload []byte) {
			if payload != nil {
				value["payload_length"] = len(payload)
			}
			raw, _ := json.Marshal(value)
			_, _ = conn.Write(append(raw, '\n'))
			if payload != nil {
				_, _ = conn.Write(payload)
			}
		}
		if fail {
			write(map[string]any{"type": "error", "data": map[string]any{"text": "model failed", "code": "synthesis-failed"}}, nil)
			return
		}
		write(map[string]any{"type": "audio-start", "data": format}, nil)
		for _, chunk := range chunks {
			write(map[string]any{"type": "audio-chunk"}, chunk)
		}
		write(map[string]any{"type": "audio-stop"}, nil)
	}()
	return listener.Addr().String()
}

func TestPreviewSynthesizesTheBoundedPhraseToWAV(t *testing.T) {
	addr := startSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 1},
		[][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}},
		false,
	)
	sampler := voicesample.New(wyoming.NewClient(addr, wyoming.WithTimeout(2*time.Second)), 2*time.Second)
	data, err := sampler.Preview(context.Background(), "/voices/jarvis.safetensors", 1.25)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(data) != 44+8 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("preview = %d bytes, not a 24 kHz PCM16 WAV", len(data))
	}
}

func TestPreviewRejectsAnUnsupportedFormat(t *testing.T) {
	addr := startSampleServer(t,
		map[string]any{"rate": 24000, "width": 2, "channels": 2},
		[][]byte{{1, 2, 3, 4}},
		false,
	)
	sampler := voicesample.New(wyoming.NewClient(addr, wyoming.WithTimeout(2*time.Second)), 2*time.Second)
	if _, err := sampler.Preview(context.Background(), "alba", 1); err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("Preview error = %v, want a format error", err)
	}
}

func TestPreviewReportsAServiceError(t *testing.T) {
	addr := startSampleServer(t, nil, nil, true)
	sampler := voicesample.New(wyoming.NewClient(addr, wyoming.WithTimeout(2*time.Second)), 2*time.Second)
	if _, err := sampler.Preview(context.Background(), "alba", 1); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("Preview error = %v, want the service error", err)
	}
}
