package completion

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Arbitrary hosts, credentials and non-HTTPS URLs never reach yt-dlp.
func TestYouTubeURLs(t *testing.T) {
	for _, text := range []string{"https://youtu.be/abc", "https://www.youtube.com/watch?v=abc"} {
		if !IsYouTube(text) {
			t.Errorf("rejected %s", text)
		}
	}
	for _, text := range []string{"http://youtu.be/abc", "https://youtube.com.evil/watch?v=a", "https://user@youtube.com/watch?v=a", "https://127.0.0.1/watch?v=a", "https://youtube.com:8443/watch?v=a", "https://youtube.com/playlist?list=x"} {
		if IsYouTube(text) {
			t.Errorf("accepted %s", text)
		}
	}
}

// Fake executables exercise stream failure cleanup and immediate cancellation.
func TestYouTubeProcesses(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"yt-dlp": "exec /bin/sleep 60", "ffmpeg": "exit 1", "pacat": "exec /bin/cat >/dev/null"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n# Test stream executable.\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	fires := 0
	if err := YouTube(ctx, "https://youtu.be/abc", "", func() { fires++ }); err == nil {
		t.Fatal("expected stream failure")
	}
	if time.Since(start) > time.Second {
		t.Fatal("processes did not stop promptly")
	}
	if fires != 0 {
		t.Fatalf("first audio fires = %d, want 0 when no PCM is written", fires)
	}
}

// The first PCM write notifies the Ring once, including when later chunks follow.
func TestYouTubeFirstAudioFiresOnce(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "pacat-args")
	scripts := map[string]string{
		"yt-dlp": "exit 0",
		"ffmpeg": "printf 'one'; printf 'two'",
		"pacat":  "printf '%s\\n' \"$@\" > \"$PACAT_ARGS\"\nexec /bin/cat >/dev/null",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n# Test stream executable.\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("PACAT_ARGS", argsPath)
	var mu sync.Mutex
	fires := 0
	err := YouTube(t.Context(), "https://youtu.be/abc", "desk", func() {
		mu.Lock()
		fires++
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 1 {
		t.Fatalf("first audio fires = %d, want 1", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--device=desk") {
		t.Fatalf("pacat args = %q, want the configured sink", args)
	}
}

// Two separate reads still notify once, and a nil callback does not panic.
func TestWritePCMSignalsTheFirstChunkOnce(t *testing.T) {
	src, w := io.Pipe()
	var dst bytes.Buffer
	fires := 0
	first := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- writePCM(src, &dst, func() {
			fires++
			close(first)
		})
	}()
	if _, err := w.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first chunk was not signalled")
	}
	if _, err := w.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fires != 1 {
		t.Fatalf("fires = %d, want 1", fires)
	}
	if dst.String() != "onetwo" {
		t.Fatalf("pcm = %q, want onetwo", dst.String())
	}

	var silent bytes.Buffer
	if err := writePCM(strings.NewReader("pcm"), &silent, nil); err != nil {
		t.Fatal(err)
	}
	if silent.String() != "pcm" {
		t.Fatalf("nil callback pcm = %q", silent.String())
	}
}
