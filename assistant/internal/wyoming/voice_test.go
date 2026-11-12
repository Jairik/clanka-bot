package wyoming_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

func TestSynthesizeSendsTheSpeechRate(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		req := readWireEvent(t, r)
		if req.Type != "synthesize" {
			t.Errorf("type = %q", req.Type)
		}
		if got := dataMap(t, req)["rate"]; got != 1.2 {
			t.Errorf("rate = %v", got)
		}
		writeWireEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
		writeWireEvent(t, conn, "audio-stop", nil)
	})
	client := wyoming.NewClient(addr)
	stream, err := client.Synthesize(context.Background(), "Hello.", wyoming.Voice{Name: "alba"}, wyoming.WithRate(1.2))
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	stream.Close()
}

func TestRateIsClampedToTheSupportedRange(t *testing.T) {
	for _, tc := range []struct{ rate, want float64 }{{9, 2.0}, {0.1, 0.5}, {1.0, 1.0}} {
		addr := fakeServer(t, func(conn net.Conn) {
			r := bufio.NewReader(conn)
			req := readWireEvent(t, r)
			if got := dataMap(t, req)["rate"]; got != tc.want {
				t.Errorf("rate %v sent as %v, want %v", tc.rate, got, tc.want)
			}
			writeWireEvent(t, conn, "audio-start", map[string]any{"rate": 24000, "width": 2, "channels": 1})
			writeWireEvent(t, conn, "audio-stop", nil)
		})
		client := wyoming.NewClient(addr)
		stream, err := client.Synthesize(context.Background(), "Hello.", wyoming.Voice{}, wyoming.WithRate(tc.rate))
		if err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		stream.Close()
	}
}

func TestVoiceInfoReportsRevisionAndBuiltins(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		req := readWireEvent(t, r)
		if req.Type != "voice-info" {
			t.Errorf("type = %q", req.Type)
		}
		writeWireEvent(t, conn, "voice-info-result", map[string]any{
			"revision": "pocket-tts 4.0.1 english abc123",
			"voices":   []string{"alba", "marius"},
			"dir":      "/voices",
		})
	})
	client := wyoming.NewClient(addr)
	info, err := client.VoiceInfo(context.Background())
	if err != nil {
		t.Fatalf("VoiceInfo: %v", err)
	}
	if info.Revision != "pocket-tts 4.0.1 english abc123" || len(info.Voices) != 2 || info.Dir != "/voices" {
		t.Fatalf("info = %+v", info)
	}
}

func TestValidateVoiceReportsSuccessAndFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		addr := fakeServer(t, func(conn net.Conn) {
			r := bufio.NewReader(conn)
			req := readWireEvent(t, r)
			if req.Type != "voice-validate" {
				t.Errorf("type = %q", req.Type)
			}
			if got := dataMap(t, req)["path"]; got != "/voices/jarvis.safetensors" {
				t.Errorf("path = %v", got)
			}
			writeWireEvent(t, conn, "voice-validated", map[string]any{"ok": true, "revision": "rev-1", "error": ""})
		})
		revision, err := wyoming.NewClient(addr).ValidateVoice(context.Background(), "/voices/jarvis.safetensors")
		if err != nil || revision != "rev-1" {
			t.Fatalf("ValidateVoice = %q, %v", revision, err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		addr := fakeServer(t, func(conn net.Conn) {
			_ = readWireEvent(t, bufio.NewReader(conn))
			writeWireEvent(t, conn, "voice-validated", map[string]any{"ok": false, "revision": "", "error": "model rejected the state"})
		})
		_, err := wyoming.NewClient(addr).ValidateVoice(context.Background(), "/voices/jarvis.safetensors")
		var serverErr *wyoming.ServerError
		if !errors.As(err, &serverErr) || serverErr.Text != "model rejected the state" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestWarmVoiceReportsSuccess(t *testing.T) {
	addr := fakeServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		req := readWireEvent(t, r)
		if req.Type != "voice-warm" {
			t.Errorf("type = %q", req.Type)
		}
		if got := dataMap(t, req)["voice"]; got != "/voices/jarvis.safetensors" {
			t.Errorf("voice = %v", got)
		}
		writeWireEvent(t, conn, "voice-warmed", map[string]any{"ok": true, "error": ""})
	})
	if err := wyoming.NewClient(addr).WarmVoice(context.Background(), "/voices/jarvis.safetensors"); err != nil {
		t.Fatalf("WarmVoice: %v", err)
	}
}
