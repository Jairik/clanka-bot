// Package completion streams remote completion media into the local Pulse sink.
package completion

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
)

// IsYouTube accepts only public YouTube video URLs, never arbitrary extractor URLs.
func IsYouTube(text string) bool {
	u, err := url.Parse(text)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "youtu.be":
		return strings.Trim(u.Path, "/") != ""
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		return u.Path == "/watch" && u.Query().Get("v") != ""
	}
	return false
}

// YouTube streams one public video into sink. onFirstAudio runs once, after the
// first PCM chunk is written to the player, and must not wait for YouTube to
// return. A nil callback is ignored. Every child is cancelled and reaped
// before this returns.
func YouTube(ctx context.Context, text, sink string, onFirstAudio func()) error {
	if !IsYouTube(text) {
		return fmt.Errorf("unsupported YouTube URL")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	download := exec.CommandContext(ctx, "yt-dlp", "--no-playlist", "--no-progress", "--socket-timeout", "10", "--retries", "1", "-f", "bestaudio", "-o", "-", "--", text)
	decode := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-i", "pipe:0", "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
	args := []string{"--raw", "--rate=48000", "--channels=2", "--format=s16le"}
	if sink != "" {
		args = append(args, "--device="+sink)
	}
	play := exec.CommandContext(ctx, "pacat", args...)
	input, err := download.StdoutPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	decode.Stdin = input
	pcm, err := decode.StdoutPipe()
	if err != nil {
		return err
	}
	defer pcm.Close()
	// Copy PCM ourselves so the first write can tell the Ring to drop its cue.
	playIn, err := play.StdinPipe()
	if err != nil {
		return err
	}
	defer playIn.Close()
	if err = play.Start(); err != nil {
		return err
	}
	if err = decode.Start(); err != nil {
		cancel()
		_ = playIn.Close()
		_ = play.Wait()
		return err
	}
	// The child owns yt-dlp's stdout. Closing the parent end lets ffmpeg see
	// EOF if the download dies.
	_ = input.Close()
	if err = download.Start(); err != nil {
		cancel()
		_ = pcm.Close()
		_ = playIn.Close()
		_ = decode.Wait()
		_ = play.Wait()
		return err
	}
	copyErr := writePCM(pcm, playIn, onFirstAudio)
	// Closing stdin is what lets pacat play out and exit.
	_ = playIn.Close()
	dErr := download.Wait()
	if dErr != nil {
		cancel()
	}
	fErr := decode.Wait()
	if fErr != nil {
		cancel()
	}
	pErr := play.Wait()
	if dErr != nil {
		return dErr
	}
	if fErr != nil {
		return fErr
	}
	if pErr != nil {
		return pErr
	}
	return copyErr
}

// writePCM copies decoded PCM to the player. The callback runs once, after the
// first successful write, and a nil callback is skipped.
func writePCM(src io.Reader, dst io.Writer, onFirstAudio func()) error {
	buf := make([]byte, 32*1024)
	signaled := false
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for len(chunk) > 0 {
				written, werr := dst.Write(chunk)
				if written > 0 && !signaled {
					signaled = true
					if onFirstAudio != nil {
						onFirstAudio()
					}
				}
				chunk = chunk[written:]
				if werr != nil {
					return werr
				}
				if written == 0 {
					return io.ErrShortWrite
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
