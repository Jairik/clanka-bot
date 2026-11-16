package wyoming

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestTranscriptPartialBufferIsCapped proves the partial buffer cannot grow
// without bound when a service sends many transcript-chunk events and never a
// final transcript: only the most recent ones are kept, so memory stays bounded
// while the last partial stays available as a fallback (#340).
func TestTranscriptPartialBufferIsCapped(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		// Drain the three opening events (transcribe, audio-start, audio-stop).
		if _, err := readEvent(r, 0); err != nil {
			return
		}
		if _, err := readEvent(r, 0); err != nil {
			return
		}
		if _, err := readEvent(r, 0); err != nil {
			return
		}
		// Send more partials than the cap allows.
		for i := 0; i < maxTranscriptChunks+50; i++ {
			if err := writeEvent(conn, 0, "transcript-chunk", map[string]any{"text": fmt.Sprintf("c-%d", i)}, nil); err != nil {
				return
			}
		}
		_ = writeEvent(conn, 0, "transcript", map[string]any{"text": "final"}, nil)
		_ = writeEvent(conn, 0, "transcript-stop", nil, nil)
	}()

	client := NewClient(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Transcribe(ctx, TranscribeOptions{}, Format{Rate: 16000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	defer stream.Close()

	text, err := stream.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if text != "final" {
		t.Fatalf("transcript = %q, want final", text)
	}
	if len(stream.partials) > maxTranscriptChunks {
		t.Fatalf("partials len = %d, want <= %d", len(stream.partials), maxTranscriptChunks)
	}
	// The most recent partial is still reachable as a fallback.
	last := stream.partials[len(stream.partials)-1]
	if last != fmt.Sprintf("c-%d", maxTranscriptChunks+50-1) {
		t.Fatalf("last partial = %q, want the most recent one", last)
	}
}
