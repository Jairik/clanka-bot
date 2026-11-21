// Package audio plays PCM on a named PulseAudio (or PipeWire) sink by piping
// raw samples into pacat, and captures the mic from a named source. Only the
// assistant container mounts the Pulse socket (ADR-0001).
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Closing a stream waits for pacat to play its buffered tail, then stops
// waiting. The bounds are what keep a sink that no longer accepts samples
// (suspended output, dead Bluetooth device, wedged Pulse server, a blocked
// client-library write) from holding Close forever: speech.SerialSink keeps
// its one-speaker gate shut until Close returns, so an unbounded wait silences
// every later reply, Filler, Cue, Ring and Announcement.
//
// Writes are not paced, so how far pacat runs ahead of the speaker is set by
// the server, not by --latency-msec=100: PipeWire's pulse server has accepted
// more than 5 s of a reply into a Bluetooth sink within half a second. Close
// therefore allows the audio still unplayed at that moment (bytes written less
// the time since the first Write) plus drainTimeout. The grace alone also
// covers the 64 KiB pipe: 2.05 s of the 16 kHz mono PCM every Cue plays
// (cmd/assistant builds the Cue sink at the capture rate), 1.4 s at 24 kHz.
var (
	// drainTimeout is the grace on top of the unplayed audio.
	drainTimeout = 4 * time.Second
	// killTimeout bounds the wait after the stream context is cancelled and
	// CommandContext has killed pacat.
	killTimeout = 1 * time.Second
)

// errDrainTimeout reports a pacat that was still running after its stdin
// closed, so Close stopped the stream instead of waiting for a drain that
// never came. The reply path logs it.
var errDrainTimeout = errors.New("audio: pacat did not drain after stdin closed")

// Format is the PCM layout expected by the sink.
type Format struct {
	Rate     int
	Width    int
	Channels int
}

// PulseSink opens one pacat stream per reply and drives pactl for volume. It
// is safe for concurrent use.
type PulseSink struct {
	// Device resolves the live EQ destination; volume remains on the configured master.
	Device func() string
	// VolumeDevice resolves the original output when EQ changes the server default.
	VolumeDevice func() string
	binary       string
	pactl        string
	device       string

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
// for the audio to drain, for at most the unplayed audio plus drainTimeout; a sink that stopped
// accepting samples is killed instead of blocking the caller. Volume and stop
// are handled by SetVolume, AdjustVolume and Stop below.
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
	device := s.device
	if s.Device != nil {
		device = s.Device()
	}
	if device != "" {
		args = append(args, "--device="+device)
	}

	// A child context lets Stop cancel only the streams this process opened.
	streamCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(streamCtx, s.binary, args...)
	// Backstop only. WaitDelay's timer starts when the child exits or its
	// context is cancelled, so it cannot bound the drain; it does bound a pipe
	// that a grandchild still holds open once pacat is gone. Keep it well
	// inside killTimeout so this bound, not our own timer, ends the wait.
	cmd.WaitDelay = killTimeout / 2
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
	w := &processWriter{
		ctx: streamCtx, cmd: cmd, stdin: stdin, stderr: stderr, cancel: cancel,
		bytesPerSecond: int64(format.Rate) * int64(format.Width) * int64(format.Channels),
	}
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

// AdjustVolume steps the sink volume by a relative percent. The target is
// capped at 0-100 so "louder" cannot climb into PulseAudio's amplification
// range, which the Dashboard cannot express. A step
// that cannot change the level writes nothing and reports applied=false, so
// callers can answer without claiming the volume moved.
func (s *PulseSink) AdjustVolume(ctx context.Context, delta int) (bool, error) {
	current, err := s.Level(ctx)
	if err != nil {
		return false, err
	}
	// An up step at or above the ceiling cannot raise the level, and clamping to
	// 100 would turn it into a drop, so it writes nothing.
	if delta > 0 && current >= 100 {
		return false, nil
	}
	target := current + delta
	if target < 0 {
		target = 0
	}
	if target > 100 {
		target = 100
	}
	if target == current {
		return false, nil
	}
	if err := s.SetVolume(ctx, target); err != nil {
		return false, err
	}
	return true, nil
}

// Level reads only the sink's level, without the mute query, so a slow or
// failing get-sink-mute cannot fail a relative volume step.
func (s *PulseSink) Level(ctx context.Context) (int, error) {
	out, err := s.runPactl(ctx, "get-sink-volume", s.target())
	if err != nil {
		return 0, err
	}
	percent, ok := parsePactlPercent(out)
	if !ok {
		return 0, fmt.Errorf("audio: pactl get-sink-volume: no percent in %q", truncate(out, 120))
	}
	return percent, nil
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
// The two queries are independent, so they run concurrently: a Bluetooth sink
// can take seconds to answer, and running them one after the other halved the
// caller's one-read budget and let a slow wake kill the second pactl call.
func (s *PulseSink) Volume(ctx context.Context) (VolumeState, error) {
	target := s.target()
	var (
		wg        sync.WaitGroup
		percent   int
		volumeErr error
		muteOut   string
		muteErr   error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		percent, volumeErr = s.Level(ctx)
	}()
	go func() {
		defer wg.Done()
		muteOut, muteErr = s.runPactl(ctx, "get-sink-mute", target)
	}()
	wg.Wait()

	// The level is the part that matters, so its error wins when both fail.
	if volumeErr != nil {
		return VolumeState{}, volumeErr
	}
	if muteErr != nil {
		return VolumeState{}, muteErr
	}
	return VolumeState{Percent: percent, Muted: strings.Contains(strings.ToLower(muteOut), "yes")}, nil
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
	device := s.device
	if s.VolumeDevice != nil {
		device = s.VolumeDevice()
	}
	if device == "" {
		return "@DEFAULT_SINK@"
	}
	return device
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
	target := s.target()
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

	// bytesPerSecond, written and firstWrite size the drain bound.
	bytesPerSecond int64
	written        atomic.Int64
	firstWrite     atomic.Int64 // UnixNano of the first accepted byte; 0 before it
}

func (w *processWriter) Write(p []byte) (int, error) {
	// A stopped or cancelled stream fails before buffering any more PCM.
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.stdin.Write(p)
	if n > 0 {
		w.firstWrite.CompareAndSwap(0, time.Now().UnixNano())
		w.written.Add(int64(n))
	}
	if err != nil {
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
		return n, fmt.Errorf("audio: pacat write: %w%s", err, w.stderrTail())
	}
	return n, nil
}

// Close drains pacat and returns its exit status. Both waits are bounded so a
// stalled sink cannot hold the serial speaker gate. Close is idempotent and
// always releases the stream and its context exactly once.
func (w *processWriter) Close() error {
	w.closeOnce.Do(func() {
		w.closeErr = w.close()
	})
	return w.closeErr
}

// close is Close's body. It returns the drain stall as an error so the reply
// path logs it instead of the speaker going quiet.
func (w *processWriter) close() (err error) {
	defer func() {
		// The stream context must outlive the drain; cancelling it here also
		// tells CommandContext that the child is no longer needed.
		if w.release != nil {
			w.release()
		}
		w.cancel()
	}()

	// Closing stdin is how pacat is told to play its buffer and exit.
	_ = w.stdin.Close()

	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()

	bound := w.drainBound(time.Now())
	select {
	case err = <-done:
	case <-time.After(bound):
		// The drain stalled. Cancelling the stream is what makes CommandContext
		// kill pacat, and only then can WaitDelay help. A caller that already
		// cancelled (Stop, Barge-in) owns the reason.
		callerStopped := w.ctx.Err()
		w.cancel()
		select {
		case err = <-done:
		case <-time.After(killTimeout):
			if callerStopped != nil {
				return callerStopped
			}
			return fmt.Errorf("%w and was still running %s after it was stopped%s",
				errDrainTimeout, killTimeout, w.stderrTail())
		}
		if callerStopped != nil {
			return callerStopped
		}
		return fmt.Errorf("%w and was stopped after %s%s", errDrainTimeout, bound.Round(time.Millisecond), w.stderrTail())
	}

	if err != nil {
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("audio: pacat: %w%s", err, w.stderrTail())
	}
	return nil
}

// drainBound is how long Close waits for pacat to exit: the audio it has
// accepted but cannot have played by now, plus drainTimeout. A sink that
// stalls is still stopped, just no earlier than a healthy one would finish.
func (w *processWriter) drainBound(now time.Time) time.Duration {
	bound := drainTimeout
	first := w.firstWrite.Load()
	if first == 0 || w.bytesPerSecond <= 0 {
		return bound
	}
	audio := time.Duration(w.written.Load() * int64(time.Second) / w.bytesPerSecond)
	if unplayed := audio - now.Sub(time.Unix(0, first)); unplayed > 0 {
		bound += unplayed
	}
	return bound
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
