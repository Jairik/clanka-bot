package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/completion"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

// youtubeAudioTimeout stops a stuck yt-dlp from holding the install request.
// A short clip under the 10 MB cap finishes well inside this.
const youtubeAudioTimeout = 2 * time.Minute

// errYouTubeTooLarge is the download-side twin of an oversized upload.
// The handler maps it to the same 413 the file upload uses.
var errYouTubeTooLarge = errors.New("youtube audio is larger than the 10 MB limit")

// YouTubeDownload is one video's audio saved as a temp MP3.
// The Sounds page removes Path and Dir after import, whether it succeeded or not.
type YouTubeDownload struct {
	// Path is the MP3 to import. Empty when the download failed before a file existed.
	Path string
	// Title is the video title yt-dlp reported. Empty means the caller names
	// the library entry from the video id instead.
	Title string
	// Dir is the temp directory that holds Path. Empty when the caller only
	// has a single file to remove, which is how tests hand back a fake MP3.
	Dir string
}

// YouTubeFetcher downloads one public YouTube video's audio.
// Tests replace it so the suite never runs yt-dlp or contacts YouTube.
type YouTubeFetcher func(ctx context.Context, videoURL string) (YouTubeDownload, error)

// soundsYouTube installs one public YouTube video as a library sound.
// The audio is downloaded once and stored through Import; later playback uses
// that file. This is not the live stream timer completion uses.
func (h *Handler) soundsYouTube(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	raw := strings.TrimSpace(r.FormValue("url"))
	// A non-video URL never starts a download.
	if !completion.IsYouTube(raw) {
		h.soundsFail(w, r, http.StatusBadRequest, "Enter a public YouTube video link.", nil)
		return
	}
	fetch := h.youtube
	if fetch == nil {
		fetch = downloadYouTubeAudio
	}
	downloaded, err := fetch(r.Context(), raw)
	defer removeYouTubeDownload(downloaded)
	if err != nil {
		if errors.Is(err, errYouTubeTooLarge) {
			h.soundsFail(w, r, http.StatusRequestEntityTooLarge, "The upload is larger than the 10 MB limit.", nil)
			return
		}
		h.logger.Warn("youtube sound download failed", "error", err.Error())
		h.soundsFail(w, r, http.StatusBadRequest, "Could not download audio from that YouTube link.", nil)
		return
	}
	info, statErr := os.Stat(downloaded.Path)
	if statErr != nil || !info.Mode().IsRegular() {
		h.logger.Warn("youtube sound unreadable", "error", fmt.Sprint(statErr))
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the downloaded audio.", nil)
		return
	}
	// Same ceiling as a file upload, checked before Import so a huge MP3 is
	// never copied into the library.
	if info.Size() > audio.MaxUploadBytes {
		h.soundsFail(w, r, http.StatusRequestEntityTooLarge, "The upload is larger than the 10 MB limit.", nil)
		return
	}
	file, err := os.Open(downloaded.Path)
	if err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the downloaded audio.", nil)
		return
	}
	defer file.Close()
	entry, err := h.sounds.Import(r.Context(), file, youtubeLibraryName(raw, downloaded.Title))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sound.ErrUploadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.soundsFail(w, r, status, err.Error(), nil)
		return
	}
	h.logger.Info("sound installed from youtube", "name", entry.Name, "duration", entry.Duration.String())
	h.soundsSuccess(w, r, flashInstalled)
}

// downloadYouTubeAudio runs yt-dlp once and leaves the MP3 in a temp directory.
// On any failure the directory is removed and the library is not touched.
func downloadYouTubeAudio(ctx context.Context, videoURL string) (out YouTubeDownload, err error) {
	ctx, cancel := context.WithTimeout(ctx, youtubeAudioTimeout)
	defer cancel()

	var dir string
	dir, err = os.MkdirTemp("", "clanka-youtube-*")
	if err != nil {
		return YouTubeDownload{}, fmt.Errorf("create youtube download dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
			out = YouTubeDownload{}
		}
	}()

	cmd := exec.CommandContext(ctx, "yt-dlp", youtubeDLArgs(dir, videoURL)...)
	cmd.Dir = dir
	// Keep yt-dlp's cache in the temp dir so a failed install leaves nothing in the home directory.
	cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"))
	// shortcut: killing the yt-dlp process group also stops ffmpeg, which it
	// spawns to write the mp3. The default cancel would leave that child running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return killErr
	}
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("youtube download timed out: %w", ctx.Err())
			return YouTubeDownload{}, err
		}
		combined := stdout.String() + stderr.String()
		if strings.Contains(combined, "max-filesize") {
			err = errYouTubeTooLarge
			return YouTubeDownload{}, err
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 300 {
			detail = detail[:300]
		}
		if detail != "" {
			err = fmt.Errorf("yt-dlp: %w: %s", err, detail)
		} else {
			err = fmt.Errorf("yt-dlp: %w", err)
		}
		return YouTubeDownload{}, err
	}
	var mp3 string
	mp3, err = findYouTubeMP3(dir)
	if err != nil {
		return YouTubeDownload{}, err
	}
	info, statErr := os.Stat(mp3)
	if statErr != nil {
		err = fmt.Errorf("stat youtube audio: %w", statErr)
		return YouTubeDownload{}, err
	}
	if info.Size() > audio.MaxUploadBytes {
		err = errYouTubeTooLarge
		return YouTubeDownload{}, err
	}
	out = YouTubeDownload{Path: mp3, Title: youtubeTitle(stdout.String()), Dir: dir}
	return out, nil
}

// youtubeDLArgs is the yt-dlp argument list for one audio install.
// The URL is a single argument after --, never joined into a shell string.
func youtubeDLArgs(dir, videoURL string) []string {
	return []string{
		"--no-playlist",
		"--no-progress",
		"--socket-timeout", "10",
		"--retries", "1",
		"--max-filesize", strconv.FormatInt(audio.MaxUploadBytes, 10),
		"-x",
		"--audio-format", "mp3",
		"--print", "after_move:title",
		"-o", filepath.Join(dir, "audio.%(ext)s"),
		"--",
		videoURL,
	}
}

// findYouTubeMP3 returns the extracted MP3 inside dir.
// yt-dlp names it from the -o template; any other single mp3 is accepted.
func findYouTubeMP3(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read youtube download: %w", err)
	}
	found := ""
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".mp3") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.Name() == "audio.mp3" {
			return path, nil
		}
		if found == "" {
			found = path
		}
	}
	if found == "" {
		return "", errors.New("youtube download produced no mp3")
	}
	return found, nil
}

// youtubeTitle keeps the first real title line yt-dlp printed.
// "NA" is yt-dlp's placeholder for a missing field, so it counts as empty.
func youtubeTitle(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "NA" {
			continue
		}
		return line
	}
	return ""
}

// youtubeLibraryName is the filename Import slugs into a library name.
// A reported title wins. An empty title falls back to the video id so the
// same video keeps the same name, and a duplicate still gets Import's suffix.
func youtubeLibraryName(videoURL, title string) string {
	title = strings.TrimSpace(title)
	if title != "" && !strings.EqualFold(title, "NA") {
		return title
	}
	if id := youtubeVideoID(videoURL); id != "" {
		return id
	}
	return "youtube"
}

// youtubeVideoID reads the id IsYouTube already accepted.
// youtu.be carries it in the path; youtube.com/watch carries it in v.
func youtubeVideoID(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if strings.EqualFold(parsed.Hostname(), "youtu.be") {
		id := strings.Trim(parsed.Path, "/")
		if cut := strings.IndexByte(id, '/'); cut >= 0 {
			id = id[:cut]
		}
		return id
	}
	return parsed.Query().Get("v")
}

// removeYouTubeDownload deletes the temp MP3 and, when set, its directory.
// A second remove is harmless, so both the downloader and the handler may call it.
func removeYouTubeDownload(dl YouTubeDownload) {
	if dl.Dir != "" {
		_ = os.RemoveAll(dl.Dir)
		return
	}
	if dl.Path != "" {
		_ = os.Remove(dl.Path)
	}
}
