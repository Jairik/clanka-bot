package audio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// fakePacat writes the arguments it received and the PCM it read, then exits.
func fakePacat(t *testing.T) (binary, argsPath, pcmPath string) {
	t.Helper()
	dir := t.TempDir()
	argsPath = filepath.Join(dir, "args.txt")
	pcmPath = filepath.Join(dir, "pcm.raw")
	binary = filepath.Join(dir, "pacat")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\ncat > '" + pcmPath + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}
	return binary, argsPath, pcmPath
}

func TestPulseSinkPlaysPCMThroughPacat(t *testing.T) {
	binary, argsPath, pcmPath := fakePacat(t)
	sink := audio.NewPulseSink(binary, "echo_dot")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	writer, err := sink.Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := writer.Write([]byte{1, 2}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := writer.Write([]byte{3, 4}); err != nil {
		t.Fatalf("Write second chunk: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	for _, want := range []string{"--format=s16le", "--rate=24000", "--channels=1", "--raw", "--device=echo_dot", "--latency-msec="} {
		if !strings.Contains(string(args), want) {
			t.Errorf("pacat args missing %q in:\n%s", want, args)
		}
	}
	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		t.Fatalf("read pcm: %v", err)
	}
	if string(pcm) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("pcm = %v, want [1 2 3 4]", pcm)
	}
}

func TestPulseSinkUsesDefaultDeviceWhenUnset(t *testing.T) {
	binary, argsPath, _ := fakePacat(t)
	sink := audio.NewPulseSink(binary, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	writer, err := sink.Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if strings.Contains(string(args), "--device=") {
		t.Fatalf("unexpected --device in:\n%s", args)
	}
}

func TestPulseSinkRejectsUnsupportedWidth(t *testing.T) {
	binary, _, _ := fakePacat(t)
	sink := audio.NewPulseSink(binary, "")
	if _, err := sink.Open(context.Background(), audio.Format{Rate: 24000, Width: 3, Channels: 1}); err == nil {
		t.Fatal("Open succeeded, want unsupported width error")
	}
}

func TestPulseSinkStopsWhenContextCancelled(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pacat")
	// Hold stdin open without reading so the writer blocks until the process dies.
	script := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pacat: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	writer, err := audio.NewPulseSink(binary, "").Open(ctx, audio.Format{Rate: 24000, Width: 2, Channels: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := writer.Write(chunk); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Write succeeded after cancel, want error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Write did not return after cancel")
	}
	if err := writer.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
}
