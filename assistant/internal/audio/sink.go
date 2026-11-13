// Package audio plays PCM on a named PulseAudio (or PipeWire) sink by piping
// raw samples into pacat, and captures the mic from a named source. Only the
// assistant container mounts the Pulse socket (ADR-0001).
package audio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Format is the PCM layout expected by the sink.
type Format struct {
	Rate     int
	Width    int
	Channels int
}

// PulseSink opens one pacat stream per reply and drives pactl for volume. It
// is safe for concurrent use.
type PulseSink struct {
	binary string
	pactl  string
	device string

	mu     sync.Mutex
	active map[*processWriter]struct{}
}

// NewPulseSink builds a sink. An empty binary uses pacat from PATH; an empty
// device uses the host's default sink. pactl is looked up beside pacat.
func NewPulseSink(binary, device string) *PulseSink {
	if binary == "" {
		binary = "pacat"
	}
	return &PulseSink{
		binary: binary,
		pactl:  pactlFor(binary),
		device: device,
		active: make(map[*processWriter]struct{}),
	}
}

// pactlFor finds the matching pulseaudio-utils control binary.
func pactlFor(pacat string) string {
	dir := filepath.Dir(pacat)
	if dir == "." || dir == "" {
		return "pactl"
	}
	return filepath.Join(dir, "pactl")
}

// Open starts pacat and returns a writer for raw PCM. Closing the writer waits
// until the audio has drained. Volume and stop are handled by SetVolume,
// AdjustVolume and Stop below.
func (s *PulseSink) Open(ctx context.Context, format Format) (io.WriteCloser, error) {
	sampleFormat, err := sampleFormat(format.Width)
	if err != nil {
		return nil, err
	}
	if format.Rate <= 0 || format.Channels <= 0 {
		return nil, fmt.Errorf("audio: invalid format %+v", format)
	}

	args := []string{
		"--client-name=clanka-assistant",
		"--stream-name=assistant-reply",
		"--format=" + sampleFormat,
		"--rate=" + strconv.Itoa(format.Rate),
		"--channels=" + strconv.Itoa(format.Channels),
		"--latency-msec=100",
		"--raw",
	}
	if s.device != "" {
		args = append(args, "--device="+s.device)
	}

	// A child context lets Stop cancel only the streams this process opened.
	streamCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(streamCtx, s.binary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("audio: pacat stdin: %w", err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("audio: start pacat: %w", err)
	}
	w := &processWriter{ctx: streamCtx, cmd: cmd, stdin: stdin, stderr: stderr, cancel: cancel}
	s.mu.Lock()
	s.active[w] = struct{}{}
	s.mu.Unlock()
	w.release = func() {
		s.mu.Lock()
		delete(s.active, w)
		s.mu.Unlock()
	}
	return w, nil
}

// SetVolume sets the sink volume to an absolute percent.
func (s *PulseSink) SetVolume(ctx context.Context, percent int) error {
	if percent < 0 || percent > 100 {
		return fmt.Errorf("audio: volume %d out of range 0-100", percent)
	}
	return s.pactlVolume(ctx, strconv.Itoa(percent)+"%")
}

// AdjustVolume changes the sink volume by a relative percent.
func (s *PulseSink) AdjustVolume(ctx context.Context, delta int) error {
	return s.pactlVolume(ctx, fmt.Sprintf("%+d%%", delta))
}

// Stop cancels the playback streams this process opened, leaving unrelated
// host processes alone.
func (s *PulseSink) Stop(_ context.Context) error {
	s.mu.Lock()
	writers := make([]*processWriter, 0, len(s.active))
	for w := range s.active {
		writers = append(writers, w)
	}
	s.mu.Unlock()
	for _, w := range writers {
		w.cancel()
	}
	return nil
}

// VolumeState is the sink's current level and mute flag. Mute is separate
// from the level so unmuting restores the previous volume.
type VolumeState struct {
	Percent int
	Muted   bool
}

// Volume reads the sink's level (the first channel's percent) and mute flag.
func (s *PulseSink) Volume(ctx context.Context) (VolumeState, error) {
	out, err := s.runPactl(ctx, "get-sink-volume", s.target())
	if err != nil {
		return VolumeState{}, err
	}
	percent, ok := parsePactlPercent(out)
	if !ok {
		return VolumeState{}, fmt.Errorf("audio: pactl get-sink-volume: no percent in %q", truncate(out, 120))
	}
	out, err = s.runPactl(ctx, "get-sink-mute", s.target())
	if err != nil {
		return VolumeState{}, err
	}
	return VolumeState{Percent: percent, Muted: strings.Contains(strings.ToLower(out), "yes")}, nil
}

// SetMute mutes or unmutes the sink without touching its level.
func (s *PulseSink) SetMute(ctx context.Context, muted bool) error {
	flag := "0"
	if muted {
		flag = "1"
	}
	_, err := s.runPactl(ctx, "set-sink-mute", s.target(), flag)
	return err
}

func (s *PulseSink) target() string {
	if s.device == "" {
		return "@DEFAULT_SINK@"
	}
	return s.device
}

func (s *PulseSink) runPactl(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, s.pactl, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("audio: pactl %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// parsePactlPercent reads the first "NN%" from pactl's volume line, for
// example "Volume: front-left: 42598 /  65% / -11.23 dB, ...".
func parsePactlPercent(out string) (int, bool) {
	end := strings.IndexByte(out, '%')
	if end < 0 {
		return 0, false
	}
	start := end
	for start > 0 && out[start-1] >= '0' && out[start-1] <= '9' {
		start--
	}
	n, err := strconv.Atoi(out[start:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *PulseSink) pactlVolume(ctx context.Context, amount string) error {
	target := s.device
	if target == "" {
		target = "@DEFAULT_SINK@"
	}
	cmd := exec.CommandContext(ctx, s.pactl, "set-sink-volume", target, amount)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("audio: pactl set-sink-volume: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sampleFormat maps a PCM sample width in bytes to a pacat format name.
func sampleFormat(width int) (string, error) {
	switch width {
	case 1:
		return "u8", nil
	case 2:
		return "s16le", nil
	case 4:
		return "s32le", nil
	default:
		return "", fmt.Errorf("audio: unsupported sample width %d bytes", width)
	}
}

// processWriter feeds pacat's stdin and reports its exit status on Close.
type processWriter struct {
	ctx       context.Context
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stderr    *lockedBuffer
	cancel    context.CancelFunc
	release   func()
	closeOnce sync.Once
	closeErr  error
}

func (w *processWriter) Write(p []byte) (int, error) {
	// A stopped or cancelled stream fails before buffering any more PCM.
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.stdin.Write(p)
	if err != nil {
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
		return n, fmt.Errorf("audio: pacat write: %w%s", err, w.stderrTail())
	}
	return n, nil
}

func (w *processWriter) Close() error {
	w.closeOnce.Do(func() {
		_ = w.stdin.Close()
		err := w.cmd.Wait()
		if w.release != nil {
			w.release()
		}
		w.cancel()
		if err != nil {
			if ctxErr := w.ctx.Err(); ctxErr != nil {
				w.closeErr = ctxErr
				return
			}
			w.closeErr = fmt.Errorf("audio: pacat: %w%s", err, w.stderrTail())
		}
	})
	return w.closeErr
}

func (w *processWriter) stderrTail() string {
	text := bytes.TrimSpace(w.stderr.Bytes())
	if len(text) == 0 {
		return ""
	}
	return ": " + string(text)
}

// lockedBuffer collects child stderr while Write and Wait may both read it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
