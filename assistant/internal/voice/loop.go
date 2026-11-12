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
	"sync/atomic"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
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

// Responder answers one utterance. It is called with the session context, so
// cancelling that context must abandon the reply (Barge-in, #25).
type Responder interface {
	Respond(ctx context.Context, req Request) error
}

// PendingDropper discards a waiting Confirmation. pipeline.Pipeline satisfies
// it; the loop calls it on Barge-in so a stale yes cannot run later.
type PendingDropper interface {
	DropPending() (tool.PendingConfirmation, bool)
}

// Announcer is the Announcement queue as the voice loop sees it. A wake word
// starts a turn, which stops unprompted speech before capture so it is not
// recorded as part of the question.
type Announcer interface {
	TurnStarted()
	TurnFinished()
}

// Ducker lowers playing music for one turn. internal/music.Ducker satisfies
// it; the restore it returns must always run.
type Ducker interface {
	Duck(ctx context.Context) (restore func(), err error)
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
	// Dropper is optional: when set, a Barge-in also drops a waiting
	// Confirmation so a stale yes cannot run later.
	Dropper PendingDropper
	// Announcer is optional: when set, a wake word stops unprompted speech
	// before capture starts.
	Announcer Announcer
	// Ducker is optional: when set, music is ducked from the wake word through
	// the end of the reply and restored afterwards (ADR-0004).
	Ducker Ducker
	// WakeCue resolves the live wake Cue once per detection. When set it
	// replaces Config.Chime, and ok=false means the role is assigned none.
	WakeCue func() (pcm []byte, format audio.Format, ok bool)
	// Observer, when set, receives each completed voice request at the same
	// moment the structured request log line is written. It feeds the
	// Dashboard Activity page; nil leaves the loop unchanged.
	Observer latency.Observer
	Logger   *slog.Logger
}

// micFrame is one captured PCM block stamped with its capture time. The stamp
// lets the session drop audio captured while the wake chime was still playing:
// those frames are the chime echoing back through the mic, not the question.
type micFrame struct {
	pcm []byte
	at  time.Time
}

// Loop is the microphone state machine. Run is called once; it is safe for the
// capture goroutine and the session code to share the frame channel.
type Loop struct {
	cfg  Config
	deps Deps

	frames chan micFrame
}

// New validates the config and returns a loop.
func New(cfg Config, deps Deps) (*Loop, error) {
	if cfg.FrameBytes <= 0 {
		return nil, errors.New("voice: frame size must be positive")
	}
	if deps.Source == nil || deps.Wake == nil || deps.STT == nil || deps.Responder == nil {
		return nil, errors.New("voice: source, wake, stt and responder are required")
	}
	if (len(cfg.Chime) > 0 || deps.WakeCue != nil) && deps.Player == nil {
		return nil, errors.New("voice: a player is required when a wake Cue is configured")
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
	l.frames = make(chan micFrame, 8)
	// The supervisor restarts a failed source (for example Pulse not ready yet)
	// so a bad start does not disable voice until the container restarts.
	go l.capture(ctx, l.frames)

	for {
		select {
		case <-ctx.Done():
			return nil
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

// listen waits for the wake word, then runs turns. A turn interrupted by the
// wake word starts the next one immediately: the wake word that barges in is
// also the request that follows.
func (l *Loop) listen(ctx context.Context) error {
	bargeIn := false
	for {
		if !bargeIn {
			detected, err := l.waitForWake(ctx)
			if err != nil {
				return err
			}
			if !detected {
				return nil // the stream ended without firing; reopen it
			}
		}
		var err error
		bargeIn, err = l.session(ctx)
		if err != nil {
			return err
		}
	}
}

// waitForWake feeds mic frames to the wake word service until it fires. The
// stream is closed on return: the session opens its own stream for playback, so
// a detection from the question itself cannot interrupt the reply.
func (l *Loop) waitForWake(ctx context.Context) (bool, error) {
	wake, err := l.deps.Wake.Open(ctx)
	if err != nil {
		return false, err
	}
	defer wake.Close()

	detections := wake.Detections()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case fr := <-l.frames:
			if err := wake.Write(fr.pcm); err != nil {
				return false, err
			}
		case detection, ok := <-detections:
			if !ok {
				if err := wake.Err(); err != nil {
					return false, err
				}
				return false, nil
			}
			l.deps.Logger.Info("voice: wake", "model", detection.Name)
			return true, nil
		}
	}
}

// session records one request: chime, listen with endpointing, transcribe, then
// speak. It reports whether the wake word interrupted the reply (Barge-in) so
// the caller starts a fresh turn without waiting for another wake word.
func (l *Loop) session(ctx context.Context) (bool, error) {
	// A wake word starts a turn: stop unprompted speech before capture, so its
	// audio is not recorded as part of the question.
	if l.deps.Announcer != nil {
		l.deps.Announcer.TurnStarted()
		defer l.deps.Announcer.TurnFinished()
	}
	// Music stays ducked for the whole turn (wake Cue, listening and reply);
	// a Barge-in turn restores and re-ducks at the turn boundary.
	if l.deps.Ducker != nil {
		restore, err := l.deps.Ducker.Duck(ctx)
		if err != nil {
			l.deps.Logger.Warn("voice: music duck", "error", err.Error())
		}
		if restore != nil {
			defer restore()
		}
	}
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The wake Cue plays while STT starts, so the user can begin speaking at
	// once. Resolve it per detection: a Dashboard write changes the next wake
	// without a restart. Endpointing waits for it to finish because the sound
	// comes back through the mic.
	wakePCM, wakeFormat := l.cfg.Chime, l.cfg.ChimeFormat
	if l.deps.WakeCue != nil {
		wakePCM, wakeFormat = nil, audio.Format{}
		if pcm, format, ok := l.deps.WakeCue(); ok {
			wakePCM, wakeFormat = pcm, format
		}
	}
	chimeDone := make(chan struct{})
	var chimeEnded atomic.Value // time.Time, stored before chimeDone closes
	if len(wakePCM) > 0 {
		go func() {
			defer close(chimeDone)
			if err := l.deps.Player.Play(sessCtx, wakePCM, wakeFormat); err != nil && sessCtx.Err() == nil {
				l.deps.Logger.Warn("voice: wake cue", "error", err.Error())
			}
			chimeEnded.Store(time.Now())
		}()
	} else {
		close(chimeDone)
	}

	stt, err := l.deps.STT.Open(sessCtx)
	if err != nil {
		return false, err
	}
	defer stt.Close()

	endpointer, err := NewEndpointer(l.cfg.Endpoint)
	if err != nil {
		return false, err
	}

	// Frames captured while the chime played are the chime echoing through the
	// mic. Drop them, including any still buffered when it ends, instead of
	// endpointing them after chimeDone and opening a 15 s utterance on the tail
	// (#18/S6). Frames captured after the chime ends are the user.
	chimeOver := len(wakePCM) == 0
	var chimeEnd time.Time
	for ended := false; !ended; {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case fr := <-l.frames:
			if !chimeOver {
				select {
				case <-chimeDone:
					chimeOver = true
					chimeEnd, _ = chimeEnded.Load().(time.Time)
				default:
					continue // still chiming: discard the echo
				}
			}
			if !chimeEnd.IsZero() && fr.at.Before(chimeEnd) {
				continue // captured during the chime, delivered after it
			}
			if err := stt.Write(fr.pcm); err != nil {
				return false, err
			}
			ended = endpointer.Add(fr.pcm)
		}
	}
	// One timeline spans endpointing, STT, routing, the Agent path and
	// playback (#27). It starts at the user's last voiced frame, not the end
	// of the silence window; the same instant is the Filler clock (#25).
	ended := time.Now()
	timeline := latency.New(tool.NewRequestID(), latency.SourceVoice, latency.WithObserver(l.deps.Observer))
	timeline.SetLastVoice(endpointer.LastSpeech())
	if last := endpointer.LastSpeech(); !last.IsZero() {
		timeline.Set(latency.StageEndpointing, ended.Sub(last))
	}
	requestCtx := latency.WithContext(sessCtx, timeline)

	endpointAttrs := []any{
		"request_id", timeline.ID(),
		"speech", endpointer.SpeechHeard(),
		"silence_ms", endpointer.Silence().Milliseconds(),
	}
	if d, ok := timeline.Duration(latency.StageEndpointing); ok {
		endpointAttrs = append(endpointAttrs, "endpointing_ms", d.Milliseconds())
	}
	l.deps.Logger.Info("voice: endpoint", endpointAttrs...)

	started := time.Now()
	transcript, err := stt.Finish()
	if err != nil {
		timeline.Complete(l.deps.Logger, latency.OutcomeFailed, err)
		return false, err
	}
	timeline.Set(latency.StageSTTFinalize, time.Since(started))
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		l.deps.Logger.Info("voice: no speech", "request_id", timeline.ID())
		timeline.Complete(l.deps.Logger, latency.OutcomeNoAudio, nil)
		return false, nil
	}
	l.deps.Logger.Info("voice: transcript",
		"request_id", timeline.ID(),
		"text", transcript,
		"stt_ms", time.Since(started).Milliseconds(),
	)

	// Wake detection stays live while the reply plays, so the user can cut in.
	// The stream starts here rather than at the wake word, so the question that
	// just ended cannot fire it.
	watch, detections := l.watch(ctx)
	if watch != nil {
		defer watch.Close()
	}

	done := make(chan error, 1)
	go func() {
		done <- l.deps.Responder.Respond(requestCtx, Request{Transcript: transcript, Ended: ended})
	}()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return false, ctx.Err()
		case err := <-done:
			if err != nil && sessCtx.Err() == nil {
				return false, err
			}
			return false, nil
		case fr := <-l.frames:
			if watch == nil {
				continue // no way to interrupt; drain the capture
			}
			if err := watch.Write(fr.pcm); err != nil {
				l.deps.Logger.Warn("voice: barge-in watch", "error", err.Error())
				watch, detections = nil, nil
			}
		case detection, ok := <-detections:
			if !ok {
				// The watch stream ended without firing; keep the reply going.
				watch, detections = nil, nil
				continue
			}
			detected := time.Now()
			l.deps.Logger.Info("voice: barge-in", "model", detection.Name)
			cancel() // abandons the agent stream, TTS and playback
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				l.deps.Logger.Warn("voice: barge-in reply", "error", err.Error())
			}
			l.deps.Logger.Info("voice: barge-in stopped", "stop_ms", time.Since(detected).Milliseconds())
			l.dropPending()
			return true, nil
		}
	}
}

// watch opens the playback-time wake stream. A failure is logged, not fatal:
// the reply still plays, just without the ability to interrupt it.
func (l *Loop) watch(ctx context.Context) (WakeStream, <-chan Detection) {
	if l.deps.Wake == nil {
		return nil, nil
	}
	wake, err := l.deps.Wake.Open(ctx)
	if err != nil {
		l.deps.Logger.Warn("voice: barge-in watch unavailable", "error", err.Error())
		return nil, nil
	}
	return wake, wake.Detections()
}

// dropPending discards a waiting Confirmation after a Barge-in.
func (l *Loop) dropPending() {
	if l.deps.Dropper == nil {
		return
	}
	if dropped, ok := l.deps.Dropper.DropPending(); ok {
		l.deps.Logger.Info("voice: barge-in dropped confirmation", "tool", dropped.Tool)
	}
}

// capture supervises the microphone: it retries a failed source with a short
// backoff and only stops when the loop's context is cancelled.
func (l *Loop) capture(ctx context.Context, frames chan<- micFrame) {
	for {
		err := l.captureOnce(ctx, frames)
		if ctx.Err() != nil {
			return
		}
		l.deps.Logger.Error("voice: capture", "error", err.Error())
		if !sleep(ctx, time.Second) {
			return
		}
	}
}

// captureOnce reads fixed-size frames from the source and publishes them.
// Frames are dropped rather than blocking when a model is finalising: recent
// audio is what matters, and a stalled capture would back up the Pulse stream.
func (l *Loop) captureOnce(ctx context.Context, frames chan<- micFrame) error {
	reader, err := l.deps.Source.Open(ctx)
	if err != nil {
		return err
	}
	defer reader.Close()

	pcm := make([]byte, l.cfg.FrameBytes)
	for {
		if _, err := io.ReadFull(reader, pcm); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("voice: capture: %w", err)
		}
		select {
		case frames <- micFrame{pcm: pcm, at: time.Now()}:
			pcm = make([]byte, l.cfg.FrameBytes)
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
