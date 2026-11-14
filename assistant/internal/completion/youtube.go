// Package completion streams remote completion media into the local Pulse sink.
package completion

import (
	"context"
	"fmt"
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

// YouTube cancels and reaps every process before releasing the Ring speaker slot.
func YouTube(ctx context.Context, text, sink string) error {
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
	play.Stdin = pcm
	if err = play.Start(); err != nil {
		return err
	}
	// Children own their pipe ends; close parent readers so failures propagate EOF/SIGPIPE.
	_ = pcm.Close()
	if err = decode.Start(); err != nil {
		cancel()
		_ = play.Wait()
		return err
	}
	_ = input.Close()
	if err = download.Start(); err != nil {
		cancel()
		_ = decode.Wait()
		_ = play.Wait()
		return err
	}
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
	return pErr
}
