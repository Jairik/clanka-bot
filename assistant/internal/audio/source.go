package audio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
)

// Source opens a continuous mono PCM stream from an input device. The voice
// loop reads fixed-size frames from it and fans them out to the wake word and
// STT services; only the assistant container owns the microphone (ADR-0001).
type Source interface {
	Open(ctx context.Context) (io.ReadCloser, error)
}

// PulseSource captures from a named PulseAudio (or PipeWire) source by piping
// parec's raw output. Pulse converts the source's native rate to the requested
// rate, so the rest of the pipeline always sees one format.
type PulseSource struct {
	binary string
	device string
	format Format
}

// NewPulseSource builds a source. An empty binary uses parec from PATH; an
// empty device uses the host's default source.
func NewPulseSource(binary, device string, format Format) *PulseSource {
	if binary == "" {
		binary = "parec"
	}
	return &PulseSource{binary: binary, device: device, format: format}
}

// Open starts parec and returns a reader for raw PCM. Cancelling ctx or closing
// the reader stops the process.
func (s *PulseSource) Open(ctx context.Context) (io.ReadCloser, error) {
	sampleFormat, err := sampleFormat(s.format.Width)
	if err != nil {
		return nil, err
	}
	if s.format.Rate <= 0 || s.format.Channels <= 0 {
		return nil, fmt.Errorf("audio: invalid capture format %+v", s.format)
	}

	args := []string{
		"--client-name=clanka-assistant",
		"--stream-name=assistant-mic",
		"--format=" + sampleFormat,
		"--rate=" + strconv.Itoa(s.format.Rate),
		"--channels=" + strconv.Itoa(s.format.Channels),
		// Keep the capture buffer short so endpointing sees recent audio.
		"--latency-msec=20",
		"--raw",
	}
	if s.device != "" {
		args = append(args, "--device="+s.device)
	}

	cmd := exec.CommandContext(ctx, s.binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("audio: parec stdout: %w", err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("audio: start parec: %w", err)
	}
	return &sourceReader{ctx: ctx, cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

// sourceReader reads parec's output and reports its exit status on Close.
type sourceReader struct {
	ctx       context.Context
	cmd       *exec.Cmd
	stdout    io.ReadCloser
	stderr    *lockedBuffer
	closeOnce sync.Once
	closeErr  error
}

func (r *sourceReader) Read(p []byte) (int, error) {
	n, err := r.stdout.Read(p)
	if err != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
		if err != io.EOF {
			return n, fmt.Errorf("audio: parec read: %w%s", err, r.stderrTail())
		}
	}
	return n, err
}

func (r *sourceReader) Close() error {
	r.closeOnce.Do(func() {
		_ = r.stdout.Close()
		if err := r.cmd.Wait(); err != nil {
			if ctxErr := r.ctx.Err(); ctxErr != nil {
				r.closeErr = ctxErr
				return
			}
			r.closeErr = fmt.Errorf("audio: parec: %w%s", err, r.stderrTail())
		}
	})
	return r.closeErr
}

func (r *sourceReader) stderrTail() string {
	text := bytes.TrimSpace(r.stderr.Bytes())
	if len(text) == 0 {
		return ""
	}
	return ": " + string(text)
}
