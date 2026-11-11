// Package voice runs the microphone side of the assistant: it captures one
// Pulse source, streams frames to the wake word service, opens a transcription
// for the request, and hands the transcript to the reply pipeline.
package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// Detection is one wake word firing.
type Detection struct {
	Name string
}

// Source opens a continuous PCM stream from the microphone.
type Source interface {
	Open(ctx context.Context) (io.ReadCloser, error)
}

// WakeService opens a wake word detection stream.
type WakeService interface {
	Open(ctx context.Context) (WakeStream, error)
}

// WakeStream accepts mic audio and reports at most one detection. Detections is
// closed when the stream ends; Err then carries the reason, if any.
type WakeStream interface {
	Write(pcm []byte) error
	Detections() <-chan Detection
	Err() error
	Close() error
}

// STTService opens a speech-to-text stream.
type STTService interface {
	Open(ctx context.Context) (STTStream, error)
}

// STTStream accepts mic audio and returns the final transcript on Finish.
type STTStream interface {
	Write(pcm []byte) error
	Finish() (string, error)
	Close() error
}

// Player plays one complete PCM buffer, such as the wake chime.
type Player interface {
	Play(ctx context.Context, pcm []byte, format audio.Format) error
}

// Responder answers a transcript. It is called with the session context, so
// cancelling that context must abandon the reply (Barge-in, #25).
type Responder interface {
	Respond(ctx context.Context, transcript string) error
}

// Config is the voice loop's fixed settings.
type Config struct {
	// FrameBytes is the PCM frame size handed to the services. 1024 bytes is
	// 32 ms of 16 kHz mono PCM16.
	FrameBytes int
	Endpoint   EndpointConfig
	// Chime plays on wake detection; nil disables it.
	Chime       []byte
	ChimeFormat audio.Format
}

// Deps are the loop's collaborators, separated from Config so tests can fake
// the services without touching the mic.
type Deps struct {
	Source    Source
	Wake      WakeService
	STT       STTService
	Player    Player
	Responder Responder
	Logger    *slog.Logger
}

// Loop is the microphone state machine. Run is called once; it is safe for the
// capture goroutine and the session code to share the frame channel.
type Loop struct {
	cfg  Config
	deps Deps

	frames chan []byte
}

// New validates the config and returns a loop.
func New(cfg Config, deps Deps) (*Loop, error) {
	if cfg.FrameBytes <= 0 {
		return nil, errors.New("voice: frame size must be positive")
	}
	if deps.Source == nil || deps.Wake == nil || deps.STT == nil || deps.Responder == nil {
		return nil, errors.New("voice: source, wake, stt and responder are required")
	}
	if len(cfg.Chime) > 0 && deps.Player == nil {
		return nil, errors.New("voice: a player is required when a chime is configured")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Loop{cfg: cfg, deps: deps}, nil
}

// Run captures audio until ctx is cancelled. A capture or service failure is
// logged and retried; only cancellation ends the loop.
func (l *Loop) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered so the capture goroutine is not blocked while a model finalises.
	l.frames = make(chan []byte, 8)
	captureErr := make(chan error, 1)
	go func() { captureErr <- l.capture(ctx, l.frames) }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-captureErr:
			if err == nil || ctx.Err() != nil {
				return nil
			}
			return err
		default:
		}

		if err := l.listen(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			l.deps.Logger.Error("voice: session", "error", err.Error())
			if !sleep(ctx, 500*time.Millisecond) {
				return nil
			}
		}
	}
}

// listen waits for the wake word. Detection is muted from the moment the wake
// fires until the reply finishes: the wake stream is closed for the session.
func (l *Loop) listen(ctx context.Context) error {
	wake, err := l.deps.Wake.Open(ctx)
	if err != nil {
		return err
	}
	defer wake.Close()

	detections := wake.Detections()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-l.frames:
			if err := wake.Write(frame); err != nil {
				return err
			}
		case detection, ok := <-detections:
			if !ok {
				if err := wake.Err(); err != nil {
					return err
				}
				return nil // stream ended without a detection; reopen
			}
			l.deps.Logger.Info("voice: wake", "model", detection.Name)
			if err := wake.Close(); err != nil {
				l.deps.Logger.Warn("voice: close wake", "error", err.Error())
			}
			return l.session(ctx)
		}
	}
}

// session records one request: chime, listen with endpointing, transcribe, then
// speak. #25 (Barge-in) keeps the wake stream open and cancels sessCtx instead.
func (l *Loop) session(ctx context.Context) error {
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The chime plays while STT starts, so the user can begin speaking at once.
	// Endpointing waits for it to finish: the chime comes back through the mic
	// and must not count as the start of the question.
	chimeDone := make(chan struct{})
	if len(l.cfg.Chime) > 0 {
		go func() {
			defer close(chimeDone)
			if err := l.deps.Player.Play(sessCtx, l.cfg.Chime, l.cfg.ChimeFormat); err != nil && sessCtx.Err() == nil {
				l.deps.Logger.Warn("voice: chime", "error", err.Error())
			}
		}()
	} else {
		close(chimeDone)
	}

	stt, err := l.deps.STT.Open(sessCtx)
	if err != nil {
		return err
	}
	defer stt.Close()

	endpointer, err := NewEndpointer(l.cfg.Endpoint)
	if err != nil {
		return err
	}

	for ended := false; !ended; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-l.frames:
			if err := stt.Write(frame); err != nil {
				return err
			}
			select {
			case <-chimeDone:
				ended = endpointer.Add(frame)
			default:
				// Still chiming; keep the audio but do not endpoint on it.
			}
		}
	}
	l.deps.Logger.Info(
		"voice: endpoint",
		"speech", endpointer.SpeechHeard(),
		"silence_ms", endpointer.Silence().Milliseconds(),
	)

	started := time.Now()
	transcript, err := stt.Finish()
	if err != nil {
		return err
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		l.deps.Logger.Info("voice: no speech")
		return nil
	}
	l.deps.Logger.Info("voice: transcript", "text", transcript, "stt_ms", time.Since(started).Milliseconds())

	done := make(chan error, 1)
	go func() { done <- l.deps.Responder.Respond(sessCtx, transcript) }()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case err := <-done:
			if err != nil && sessCtx.Err() == nil {
				return err
			}
			return nil
		case <-l.frames:
			// Muted: drain the capture so the next session starts on fresh audio.
		}
	}
}

// capture reads fixed-size frames from the source and publishes them. Frames
// are dropped rather than blocking when a model is finalising: recent audio is
// what matters, and a stalled capture would back up the Pulse stream.
func (l *Loop) capture(ctx context.Context, frames chan<- []byte) error {
	reader, err := l.deps.Source.Open(ctx)
	if err != nil {
		return err
	}
	defer reader.Close()

	frame := make([]byte, l.cfg.FrameBytes)
	for {
		if _, err := io.ReadFull(reader, frame); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("voice: capture: %w", err)
		}
		select {
		case frames <- frame:
			frame = make([]byte, l.cfg.FrameBytes)
		default:
			// Consumer is busy; drop this frame.
		}
	}
}

// sleep waits for d or until ctx is cancelled, reporting whether it waited.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
