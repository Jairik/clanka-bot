package wyoming

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// raceServer is a local TCP peer that drains everything the client sends and
// never answers. It stands in for a stalled Wyoming service without touching a
// real ML container.
func raceServer(t *testing.T) string {
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
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()
	return ln.Addr().String()
}

// raceWriteClose runs a write loop against a close loop, then requires Close to
// stay idempotent. Run it with -race: a writer reading a close flag without the
// atomic fails the package test.
func raceWriteClose(t *testing.T, write func() error, closeStream func() error) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 200; i++ {
			if err := write(); err != nil {
				return // a write after Close failing is the documented contract
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 25; i++ {
			if err := closeStream(); err != nil {
				t.Errorf("Close: %v", err)
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	if err := closeStream(); err != nil {
		t.Fatalf("Close after the race: %v", err)
	}
}

// assertCloseUnblocksRead starts a blocking read, closes the stream, and
// requires the read to return an error promptly rather than hang.
func assertCloseUnblocksRead(t *testing.T, read func() error, closeStream func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- read() }()
	time.Sleep(50 * time.Millisecond)
	if err := closeStream(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending read returned no error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the pending read")
	}
}

// TestCloseIsRaceFreeWithConcurrentWrites covers #261: Close may run on another
// goroutine than Write, and Next/Finish must be unblocked by it.
func TestCloseIsRaceFreeWithConcurrentWrites(t *testing.T) {
	client := NewClient(raceServer(t), WithTimeout(5*time.Second))
	ctx := context.Background()
	format := Format{Rate: 16000, Width: 2, Channels: 1}

	t.Run("DetectStream", func(t *testing.T) {
		stream, err := client.Detect(ctx, nil, format)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		raceWriteClose(t, func() error { return stream.Write([]byte{1, 2}) }, stream.Close)
	})

	t.Run("TranscriptStream", func(t *testing.T) {
		stream, err := client.Transcribe(ctx, TranscribeOptions{}, format)
		if err != nil {
			t.Fatalf("Transcribe: %v", err)
		}
		raceWriteClose(t, func() error { return stream.Write([]byte{1, 2}) }, stream.Close)
	})
}

// TestCloseUnblocksPendingReads checks the other half of #261: the atomic swap
// still calls stopContext and closes the connection, so a blocked read returns.
func TestCloseUnblocksPendingReads(t *testing.T) {
	client := NewClient(raceServer(t), WithTimeout(time.Minute))
	ctx := context.Background()
	format := Format{Rate: 16000, Width: 2, Channels: 1}

	t.Run("DetectStream.Next", func(t *testing.T) {
		stream, err := client.Detect(ctx, nil, format)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		assertCloseUnblocksRead(t, func() error {
			_, _, err := stream.Next()
			return err
		}, stream.Close)
	})

	t.Run("TranscriptStream.Finish", func(t *testing.T) {
		stream, err := client.Transcribe(ctx, TranscribeOptions{}, format)
		if err != nil {
			t.Fatalf("Transcribe: %v", err)
		}
		assertCloseUnblocksRead(t, func() error {
			_, err := stream.Finish()
			return err
		}, stream.Close)
	})
}
