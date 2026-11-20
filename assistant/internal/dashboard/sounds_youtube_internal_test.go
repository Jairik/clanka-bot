package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// TestYouTubeDLArgsKeepTheURLAfterTheDoubleDash locks the install command to
// an argument list. A URL must not be concatenated into a shell string.
func TestYouTubeDLArgsKeepTheURLAfterTheDoubleDash(t *testing.T) {
	raw := "https://www.youtube.com/watch?v=abc123"
	args := youtubeDLArgs("/tmp/clanka-youtube", raw)
	joined := strings.Join(args, "\n")
	for _, want := range []string{"--no-playlist", "--no-progress", "--socket-timeout", "10", "--retries", "1", "-x", "--audio-format", "mp3", "--max-filesize", strconv.FormatInt(audio.MaxUploadBytes, 10)} {
		if !containsArg(args, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if args[len(args)-2] != "--" || args[len(args)-1] != raw {
		t.Fatalf("URL is not a single argument after --: %q", args[len(args)-2:])
	}
	if strings.Contains(strings.Join(args[:len(args)-1], " "), raw) {
		t.Fatal("URL was copied into an earlier argument")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestYouTubeLibraryNameUsesTitleOrVideoID(t *testing.T) {
	if got := youtubeLibraryName("https://www.youtube.com/watch?v=abc", "Birds at Dawn"); got != "Birds at Dawn" {
		t.Fatalf("title name = %q", got)
	}
	if got := youtubeLibraryName("https://youtu.be/dQw4w9WgXcQ", "  "); got != "dQw4w9WgXcQ" {
		t.Fatalf("empty title = %q, want the video id", got)
	}
	if got := youtubeLibraryName("https://m.youtube.com/watch?v=abc_def", "NA"); got != "abc_def" {
		t.Fatalf("NA title = %q, want the video id", got)
	}
	if got := youtubeTitle("\nNA\nBirds at Dawn\n"); got != "Birds at Dawn" {
		t.Fatalf("printed title = %q", got)
	}
}

// TestDownloadYouTubeAudioUsesAFakeBinary exercises yt-dlp's argument list,
// the size cap and temp cleanup without contacting YouTube or the real binary.
func TestDownloadYouTubeAudioUsesAFakeBinary(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "yt-dlp")
	body := `#!/bin/sh
printf '%s\n' "$@" > "$YTDLP_ARGS"
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    out=$arg
  fi
  prev=$arg
done
case "$out" in
  */*) dest=${out%/*} ;;
  *) dest=. ;;
esac
if [ -n "$YTDLP_MP3" ]; then
  cp "$YTDLP_MP3" "$dest/audio.mp3"
elif [ -n "$YTDLP_BYTES" ]; then
  head -c "$YTDLP_BYTES" /dev/zero > "$dest/audio.mp3"
fi
if [ -n "$YTDLP_TITLE" ]; then
  printf '%s\n' "$YTDLP_TITLE"
fi
exit "${YTDLP_EXIT:-0}"
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	argsPath := filepath.Join(dir, "args")
	t.Setenv("YTDLP_ARGS", argsPath)

	t.Run("success", func(t *testing.T) {
		mp3, err := filepath.Abs(filepath.Join("..", "audio", "testdata", "tone.mp3"))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("YTDLP_MP3", mp3)
		t.Setenv("YTDLP_TITLE", "Birds at Dawn")
		t.Setenv("YTDLP_BYTES", "")
		t.Setenv("YTDLP_EXIT", "0")
		before := youtubeTempNames()
		got, err := downloadYouTubeAudio(t.Context(), "https://youtu.be/abc123")
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Birds at Dawn" || got.Path == "" || got.Dir == "" {
			t.Fatalf("download = %+v", got)
		}
		info, statErr := os.Stat(got.Path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Size() == 0 {
			t.Fatal("downloaded mp3 is empty")
		}
		removeYouTubeDownload(got)
		if _, err := os.Stat(got.Dir); !os.IsNotExist(err) {
			t.Fatalf("temp dir still present: %v", err)
		}
		assertNoNewYouTubeTemps(t, before)
		assertYouTubeArgs(t, argsPath, "https://youtu.be/abc123")
	})

	t.Run("failure removes the temp dir", func(t *testing.T) {
		t.Setenv("YTDLP_MP3", "")
		t.Setenv("YTDLP_TITLE", "")
		t.Setenv("YTDLP_BYTES", "")
		t.Setenv("YTDLP_EXIT", "1")
		before := youtubeTempNames()
		if _, err := downloadYouTubeAudio(t.Context(), "https://www.youtube.com/watch?v=abc123"); err == nil {
			t.Fatal("expected yt-dlp failure")
		}
		assertNoNewYouTubeTemps(t, before)
	})

	t.Run("oversize file is rejected and removed", func(t *testing.T) {
		t.Setenv("YTDLP_MP3", "")
		t.Setenv("YTDLP_TITLE", "Too Big")
		t.Setenv("YTDLP_BYTES", strconv.FormatInt(audio.MaxUploadBytes+1, 10))
		t.Setenv("YTDLP_EXIT", "0")
		before := youtubeTempNames()
		_, err := downloadYouTubeAudio(t.Context(), "https://youtu.be/abc123")
		if !errors.Is(err, errYouTubeTooLarge) {
			t.Fatalf("error = %v, want the size limit", err)
		}
		assertNoNewYouTubeTemps(t, before)
	})
}

func assertYouTubeArgs(t *testing.T, path, videoURL string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != videoURL {
		t.Fatalf("recorded args = %q, want -- then the URL", args)
	}
}

func youtubeTempNames() map[string]bool {
	names := map[string]bool{}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return names
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "clanka-youtube-") {
			names[entry.Name()] = true
		}
	}
	return names
}

func assertNoNewYouTubeTemps(t *testing.T, before map[string]bool) {
	t.Helper()
	for name := range youtubeTempNames() {
		if !before[name] {
			t.Fatalf("left temp dir %s", name)
		}
	}
}
