package audio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stalledPacat writes a fake pacat that ignores stdin, records its own pid and
// keeps running. That is what a suspended sink, a dead Bluetooth device or a
// wedged Pulse server leaves behind: the write side closes, the process never
// exits, so an unbounded Close would wait forever.
func stalledPacat(t *testing.T) (binary, pidPath string) {
	t.Helper()
	dir := t.TempDir()
	pidPath = filepath.Join(dir, "pid")
	binary = filepath.Join(dir, "pacat")
	script := "#!/bin/sh\necho $$ > '" + pidPath + "'\nexec sleep 30\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	return binary, pidPath
}

// shortDeadlines shrinks Close's bounds so the timeout path is cheap to test.
func shortDeadlines(t *testing.T) {
	t.Helper()
	drain, kill := drainTimeout, killTimeout
	drainTimeout = 100 * time.Millisecond
	killTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		drainTimeout = drain
		killTimeout = kill
	})
}

func TestCloseStopsAPacatThatStopsDraining(t *testing.T) {
	shortDeadlines(t)
	binary, pidPath := stalledPacat(t)
	sink := NewPulseSink(binary, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	writer, err := sink.Open(ctx, Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := writer.Write([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	started := time.Now()
	closeErr := writer.Close()
	elapsed := time.Since(started)

	if !errors.Is(closeErr, errDrainTimeout) {
		t.Fatalf("Close = %v, want a drain-stall error", closeErr)
	}
	// The drain deadline plus the kill window, with room for a loaded runner.
	if limit := drainTimeout + killTimeout + time.Second; elapsed > limit {
		t.Fatalf("Close took %s, want at most %s", elapsed, limit)
	}

	// release ran exactly once, so Stop no longer tracks the dead stream.
	sink.mu.Lock()
	active := len(sink.active)
	sink.mu.Unlock()
	if active != 0 {
		t.Fatalf("active streams = %d after Close, want 0", active)
	}
	// The stream context is cancelled, so a Write after Close fails instead of
	// blocking on a dead pipe.
	if _, err := writer.Write([]byte{9}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Write after Close = %v, want context.Canceled", err)
	}
	waitForExit(t, pidPath)

	// Close stays idempotent and keeps the first error.
	if again := writer.Close(); !errors.Is(again, errDrainTimeout) {
		t.Fatalf("second Close = %v, want the first drain-stall error", again)
	}
}

// TestCloseKeepsContextCancelTheReason proves a Barge-in or Stop during a stuck
// drain still reports context.Canceled rather than blaming pacat.
func TestCloseKeepsContextCancelTheReason(t *testing.T) {
	shortDeadlines(t)
	binary, _ := stalledPacat(t)
	sink := NewPulseSink(binary, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writer, err := sink.Open(ctx, Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Cancel while the child ignores its stdin, so the cancel is what ends it.
	cancel()
	if closeErr := writer.Close(); !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", closeErr)
	}
}

// TestCloseStalledStreamLeaksNothing repeats the stalled drain and checks that
// neither a goroutine nor a file descriptor survives it. A leaked Wait
// goroutine would leave the reply's pipes open for the life of the process.
func TestCloseStalledStreamLeaksNothing(t *testing.T) {
	shortDeadlines(t)
	binary, _ := stalledPacat(t)
	sink := NewPulseSink(binary, "")
	format := Format{Rate: 24000, Width: 2, Channels: 1}

	// Warm up once so the first stream's own runtime wakeups are not counted.
	openStalledStream(t, sink, format)

	goroutines := runtime.NumGoroutine()
	descriptors := openDescriptors()

	for i := 0; i < 3; i++ {
		openStalledStream(t, sink, format)
	}

	// The runtime reaps an exiting goroutine asynchronously; poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if runtime.NumGoroutine() <= goroutines && openDescriptors() <= descriptors {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stalled Close leaked resources: goroutines %d -> %d, fds %d -> %d",
				goroutines, runtime.NumGoroutine(), descriptors, openDescriptors())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func openStalledStream(t *testing.T, sink *PulseSink, format Format) {
	t.Helper()
	writer, err := sink.Open(context.Background(), format)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := writer.Write(make([]byte, 128)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if closeErr := writer.Close(); !errors.Is(closeErr, errDrainTimeout) {
		t.Fatalf("Close = %v, want a drain-stall error", closeErr)
	}
}

// openDescriptors counts this process's open descriptors. It returns -1 when
// the platform has no /proc, which keeps the comparison from crying leak on a
// platform that cannot answer.
func openDescriptors() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// waitForExit fails when the child is still alive. Wait has already reaped it
// by the time Close returns, so signal 0 answers ESRCH.
func waitForExit(t *testing.T, pidPath string) {
	t.Helper()
	text, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read fake pacat pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(text)))
	if err != nil || pid <= 0 {
		t.Fatalf("fake pacat pid = %q: %v", text, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		process, err := os.FindProcess(pid)
		if err != nil || process.Signal(syscall.Signal(0)) != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pacat pid %d is still running after Close", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A Cue plays at the 16 kHz capture rate, where a full 64 KiB pipe holds 2.048 s
// of audio. The drain bound must cover that honest tail, not the 1.4 s 24 kHz
// case, or a long library sound is killed mid-play and the Ring reports a stop.
func TestDrainBoundCoversTheSixteenKilohertzCueTail(t *testing.T) {
	const (
		pipeBytes      = 64 * 1024
		cueRate        = 16000
		bytesPerSample = 2
	)
	tail := time.Duration(pipeBytes/bytesPerSample/cueRate) * time.Second
	if drainTimeout <= tail {
		t.Fatalf("drainTimeout %s does not cover the %s 16 kHz cue tail", drainTimeout, tail)
	}
}
