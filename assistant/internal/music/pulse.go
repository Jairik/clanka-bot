package music

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// PulseStreams sets the volume of the Pulse sink inputs that belong to the
// music receiver, found by application name. It shells out to pactl like the
// Assistant's other Pulse code, so it needs the same Pulse socket.
type PulseStreams struct {
	pactl string
	match string
}

// NewPulseStreams builds the stream controller. pactl is looked up beside the
// given pacat binary (or on PATH); match is a case-insensitive substring of
// the stream's application name or process binary, "spotify" when empty.
func NewPulseStreams(pacat, match string) *PulseStreams {
	if strings.TrimSpace(match) == "" {
		match = "spotify"
	}
	pactl := "pactl"
	if dir := filepath.Dir(pacat); pacat != "" && dir != "." {
		pactl = filepath.Join(dir, "pactl")
	}
	return &PulseStreams{pactl: pactl, match: strings.ToLower(match)}
}

// sinkInput is the part of `pactl --format=json list sink-inputs` we read.
type sinkInput struct {
	Index      int               `json:"index"`
	Properties map[string]string `json:"properties"`
}

// SetPercent sets every matching stream to percent. No matching stream is not
// an error: music that is not playing has nothing to duck.
func (p *PulseStreams) SetPercent(ctx context.Context, percent int) error {
	out, err := exec.CommandContext(ctx, p.pactl, "--format=json", "list", "sink-inputs").Output()
	if err != nil {
		return fmt.Errorf("music: list sink inputs: %w", err)
	}
	var inputs []sinkInput
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&inputs); err != nil {
		return fmt.Errorf("music: parse sink inputs: %w", err)
	}
	var firstErr error
	for _, in := range inputs {
		if !p.matches(in) {
			continue
		}
		arg := strconv.Itoa(percent) + "%"
		cmd := exec.CommandContext(ctx, p.pactl, "set-sink-input-volume", strconv.Itoa(in.Index), arg)
		if err := cmd.Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("music: set sink input volume: %w", err)
		}
	}
	return firstErr
}

// matches reports whether the stream belongs to the music player. Soloist's
// Pulse application name is "spotify"; the Assistant's own reply stream is
// "clanka-assistant", so it is never ducked. media.name is deliberately
// ignored: a browser playing the Spotify web player must not be ducked.
func (p *PulseStreams) matches(in sinkInput) bool {
	for _, key := range []string{"application.name", "application.process.binary"} {
		if strings.Contains(strings.ToLower(in.Properties[key]), p.match) {
			return true
		}
	}
	return false
}

// Present conservatively detects receiver streams, including playback started outside the Assistant.
func (p *PulseStreams) Present(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, p.pactl, "--format=json", "list", "sink-inputs").Output()
	if err != nil {
		return false, err
	}
	var inputs []sinkInput
	if err := json.Unmarshal(out, &inputs); err != nil {
		return false, err
	}
	for _, input := range inputs {
		if p.matches(input) {
			return true, nil
		}
	}
	return false, nil
}
