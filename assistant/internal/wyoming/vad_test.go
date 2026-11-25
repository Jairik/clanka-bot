package wyoming

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func startFakeVADServer(t *testing.T, handler func(conn net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				handler(c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestOpenVADAndScore(t *testing.T) {
	scores := []float64{0.91, 0.08}
	addr := startFakeVADServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		ev, err := readEvent(r, 2*time.Second)
		if err != nil || ev.Type != "vad-start" {
			return
		}
		_ = writeEvent(conn, time.Second, "vad-ready", map[string]any{"rate": 16000}, nil)
		for _, prob := range scores {
			ev, err := readEvent(r, 2*time.Second)
			if err != nil || ev.Type != "audio-chunk" {
				return
			}
			_ = writeEvent(conn, time.Second, "vad-score", map[string]any{"probability": prob}, nil)
		}
	})

	client := NewClient(addr, WithTimeout(2*time.Second))
	stream, err := client.OpenVAD(context.Background(), Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("OpenVAD: %v", err)
	}
	defer stream.Close()

	for i, want := range scores {
		got, err := stream.Score([]byte{0x00, 0x40, 0x00, 0xc0})
		if err != nil {
			t.Fatalf("Score[%d]: %v", i, err)
		}
		if got != want {
			t.Fatalf("Score[%d] = %v, want %v", i, got, want)
		}
	}
}

func TestOpenVADUnavailableReturnsServerError(t *testing.T) {
	addr := startFakeVADServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		if _, err := readEvent(r, 2*time.Second); err != nil {
			return
		}
		_ = writeEvent(conn, time.Second, "error", map[string]any{
			"text": "voice activity detection is unavailable",
			"code": "vad-unavailable",
		}, nil)
	})

	client := NewClient(addr, WithTimeout(2*time.Second))
	_, err := client.OpenVAD(context.Background(), Format{Rate: 16000, Width: 2, Channels: 1})
	if err == nil {
		t.Fatal("OpenVAD succeeded when server reported vad-unavailable")
	}
	if !strings.Contains(err.Error(), "vad-unavailable") {
		t.Fatalf("error = %v, want vad-unavailable", err)
	}
}
