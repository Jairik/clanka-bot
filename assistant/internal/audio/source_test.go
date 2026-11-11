package audio_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// fakeParec emits canned PCM, then waits so the caller controls when it ends.
func fakeParec(t *testing.T) (binary, argsPath string) {
	t.Helper()
	dir := t.TempDir()
	argsPath = filepath.Join(dir, "args.txt")
	binary = filepath.Join(dir, "parec")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\nprintf '\\001\\002\\003\\004'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake parec: %v", err)
	}
	return binary, argsPath
}

func TestPulseSourceCapturesFromTheNamedDevice(t *testing.T) {
	binary, argsPath := fakeParec(t)
	source := audio.NewPulseSource(binary, "webcam", audio.Format{Rate: 16000, Width: 2, Channels: 1})

	ctx, cancel := context.WithCancel(context.Background())
	reader, err := source.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	buf := make([]byte, 4)
	if _, err := ioReadFull(reader, buf); err != nil {
		t.Fatalf("read captured PCM: %v", err)
	}
	if string(buf) != "\x01\x02\x03\x04" {
		t.Fatalf("captured %v, want the fake PCM", buf)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	for _, want := range []string{"--format=s16le", "--rate=16000", "--channels=1", "--raw", "--device=webcam", "--latency-msec="} {
		if !strings.Contains(string(args), want) {
			t.Errorf("parec args missing %q in:\n%s", want, args)
		}
	}

	cancel()
	if err := reader.Close(); err == nil {
		t.Log("close returned nil after cancel; parec exited on its own")
	}
}

func TestPulseSourceUsesDefaultDeviceWhenUnset(t *testing.T) {
	binary, argsPath := fakeParec(t)
	source := audio.NewPulseSource(binary, "", audio.Format{Rate: 16000, Width: 2, Channels: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reader, err := source.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()

	args := waitForFile(t, argsPath)
	if strings.Contains(string(args), "--device=") {
		t.Fatalf("unexpected --device in:\n%s", args)
	}
}

func TestPulseSourceRejectsUnsupportedWidth(t *testing.T) {
	source := audio.NewPulseSource("parec", "", audio.Format{Rate: 16000, Width: 3, Channels: 1})
	if _, err := source.Open(context.Background()); err == nil {
		t.Fatal("Open succeeded, want unsupported width error")
	}
}

// waitForFile waits for the fake tool to write its argument list.
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ioReadFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
