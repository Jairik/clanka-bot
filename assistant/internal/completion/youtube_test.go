package completion

import (
	"context"
	"os"
	"path/filepath"
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
	if err := YouTube(ctx, "https://youtu.be/abc", ""); err == nil {
		t.Fatal("expected stream failure")
	}
	if time.Since(start) > time.Second {
		t.Fatal("processes did not stop promptly")
	}
}
