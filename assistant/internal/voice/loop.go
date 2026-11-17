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
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// Detection is one wake word firing.
type Detection struct {
	Name string
}

// WakeCommand handles one non-wake detection, such as #81's volume_up or
// volume_down. It runs on the loop goroutine, so it must be quick; it never
// opens STT and never starts a turn.
type WakeCommand func(ctx context.Context, name string)

// Source opens a continuous PCM stream from the microphone.
type Source interface {
	Open(ctx context.Context) (io.ReadCloser, error)
}

// WakeService opens a wake word detection stream.
type WakeService interface {
	Open(ctx context.Context) (WakeStream, error)
}

// Thresholds is the live per-model threshold source: the map a detect stream
// sends and a cheap signature that changes exactly when an effective value
// does. The loop compares the signature while a stream is open and reopens the
// stream when it changes, so an edit applies to the next detection instead of
// the next turn.
type Thresholds interface {
	Map() map[string]float64
	Signature() string
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
	// Refused reports a server refusal (for example a busy error) seen while the
	// utterance is still streaming, so the loop can stop early instead of writing
	// the whole request into a refused session (#368).
	Refused() <-chan error
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
	// BusyMessage is spoken when the speech-to-text service refuses the turn as
	// busy instead of transcribing it. Empty selects DefaultBusyMessage (#368).
	BusyMessage string
}

// Deps are the loop's collaborators, separated from Config so tests can fake
// the services without touching the mic.
type Deps struct {
	Source Source
	Wake   WakeService
	STT    STTService
	Player Player
	// Responder answers one utterance that followed a wake word.
	Responder Responder
	// WakeWords is the allowlist of detection names that may start a turn.
	// Production always sets it from wakeword.models. Empty accepts any name,
	// which keeps a minimal test setup (or a service-only deployment) working;
	// it is never the configured path. A name that also has a WakeCommands
	// entry is dispatched as a command, not a wake word.
	WakeWords []string
	// Thresholds, when set, is the live per-model threshold source. Its
	// signature is checked as frames arrive; a change closes the open stream
	// without firing and the loop reopens it with the new map. Nil keeps a
	// direct WakeService fixture unchanged.
	Thresholds Thresholds
	// WakeCommands handles non-wake detections such as #81's volume_up. A name
	// in this map is dispatched and the loop keeps listening; it never starts
	// an STT turn. A command model must also be requested from the service, so
	// main adds these names to the detect allowlist.
	WakeCommands map[string]WakeCommand
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
	// Stats, when set, counts false wakes for threshold tuning (#299). The loop
	// records total wakes and the no-speech kind; the main observer fan-out
	// records the unroutable kind on the same value. Nil leaves counting off.
	Stats *Stats
	// FollowUp returns the live no-wake listening window; zero disables it.
	FollowUp func() time.Duration
	// Listening reports whether wake-word listening is active. When false,
	// wake detection is paused, no detect streams are opened, and stale frames
	// are drained. Nil reports true (always listening).
	Listening func() bool
	// RingActive reports whether a Timer or Alarm Ring currently owns the
	// floor (focus.Ring is held). When true the wake Cue is skipped so it does
	// not play on top of a ringing alert.
	RingActive func() bool
	// Speaker speaks one short phrase, used to tell the user the turn was
	// refused as busy. Nil leaves the refusal silent except for the log line
	// (#368).
	Speaker Speaker
	Logger  *slog.Logger
}

// Speaker speaks one short phrase. speech.Speaker satisfies it through SayText.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// micFrame is one captured PCM block stamped with its capture time. The stamp
// lets the session drop audio captured while the wake chime was still playing:
// those frames are the chime echoing back through the mic, not the question.
type micFrame struct {
	pcm []byte
	at  time.Time
}

// DefaultBusyMessage is spoken when the speech-to-text service refuses the turn
// as busy. It is clearer than the generic "unavailable" line because the model
// is simply held by another session and will free up (#368).
const DefaultBusyMessage = "Speech recognition is busy. Try again in a moment."

// PrerollFrameCount is the fixed number of captured frames the loop holds before
// it has heard speech, so the onset of the phrase reaches STT without streaming
// the silent pre-utterance of a false wake (#213). It is a hard bound derived
// from FrameBytes and the sample rate, never the utterance length: MinSpeech is
// 200 ms, so the endpointer does not report speech until that much above-
// threshold audio has accumulated, and a zero pre-roll would clip the first word.
// At the production frame size (1024 bytes, 32 ms at 16 kHz) the bound is 16
// frames, about 512 ms.
func PrerollFrameCount(frameBytes, rate int) int {
	if rate <= 0 || frameBytes < 2 {
		return 0
	}
	frameDur := float64(frameBytes/2) / float64(rate) // seconds per frame
	const target = 0.5                                  // 500 ms
	n := int(math.Ceil(target / frameDur))
	if n < 1 {
		n = 1
	}
	return n
}

// Loop is the microphone state machine. Run is called once; it is safe for the
// capture goroutine and the session code to share the frame channel.
type Loop struct {
	cfg  Config
	deps Deps

	frames         chan micFrame
	sampleMu       sync.Mutex
	sampling       bool
	sampleFrames   chan []byte
	sampleOverflow bool
	inTurn         bool
	followUntil    time.Time
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
	if cfg.BusyMessage == "" {
		cfg.BusyMessage = DefaultBusyMessage
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
	pending := ""
	haveTurn := false
	for {
		if !l.isListening() {
			haveTurn = false
			l.followUntil = time.Time{}
		}
		if !haveTurn {
			name, detected, err := l.waitForWake(ctx)
			if err != nil {
				return err
			}
			if !detected {
				return nil // the stream ended without firing; reopen it
			}
			if !l.isListening() {
				continue
			}
			pending = name
			haveTurn = true
		}
		l.sampleMu.Lock()
		if l.sampling {
			l.sampleMu.Unlock()
			haveTurn = false
			continue
		}
		l.inTurn = true
		l.sampleMu.Unlock()
		// A wake word (including a Barge-in one) that actually starts a turn is
		// the denominator of the false-wake rate. An empty pending name is a
		// no-wake follow-up turn and is ignored by Stats.Wake.
		l.deps.Stats.Wake(pending)
		bargeIn, name, err := l.session(ctx, pending)
		l.sampleMu.Lock()
		l.inTurn = false
		l.sampleMu.Unlock()
		if err != nil {
			return err
		}
		if bargeIn {
			// The wake word that interrupted the reply is the next request.
			pending = name
			haveTurn = true
			continue
		}
		pending = ""
		haveTurn = false
		// Only a completed spoken turn can open the next listening window.
		if !l.followUntil.IsZero() && time.Now().Before(l.followUntil) && l.isListening() {
			haveTurn = true
		}
	}
}

// waitForWake feeds mic frames to the wake word service until a wake word
// fires. Command detections are dispatched and the stream is reopened; an
// unlisted name is logged and ignored. The stream is closed on return: the
// session opens its own stream for playback, so a detection from the question
// itself cannot interrupt the reply.
func (l *Loop) waitForWake(ctx context.Context) (string, bool, error) {
	for {
		if !l.isListening() {
			l.drainFrames()
			if !sleep(ctx, 200*time.Millisecond) {
				return "", false, ctx.Err()
			}
			l.drainFrames()
			continue
		}
		// Read the signature before opening, so the map the service receives
		// and the value compared against it describe the same edit.
		var signature string
		if l.deps.Thresholds != nil {
			signature = l.deps.Thresholds.Signature()
		}
		name, fired, err := func() (string, bool, error) {
			l.drainFrames()
			wake, err := l.deps.Wake.Open(ctx)
			if err != nil {
				return "", false, err
			}
			defer wake.Close()
			return l.readWake(ctx, wake, signature)
		}()
		if errors.Is(err, errThresholdsChanged) {
			l.deps.Logger.Info("voice: wake thresholds changed; reopening detect stream")
			continue
		}
		if err != nil {
			return "", false, err
		}
		if !fired {
			return "", false, nil
		}
		if !l.isListening() {
			continue
		}
		l.sampleMu.Lock()
		sampling := l.sampling
		l.sampleMu.Unlock()
		if sampling {
			continue
		}
		if l.dispatchCommand(ctx, name) {
			continue
		}
		if !l.isWakeWord(name) {
			l.deps.Logger.Warn("voice: wake ignored", "model", name)
			continue
		}
		l.deps.Logger.Info("voice: wake", "model", name)
		return name, true, nil
	}
}

// errThresholdsChanged ends one detect stream without firing so waitForWake
// reopens it with the map the Settings page now reports. It is not a failure:
// the loop keeps listening.
var errThresholdsChanged = errors.New("voice: wake thresholds changed")

// readWake feeds one open detect stream until it fires or ends. signature is
// the value read before Open; a frame that arrives after the effective
// thresholds moved ends the stream instead of feeding a stale model.
func (l *Loop) readWake(ctx context.Context, wake WakeStream, signature string) (string, bool, error) {
	detections := wake.Detections()
	for {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case fr := <-l.frames:
			if !l.isListening() {
				return "", false, nil
			}
			if l.deps.Thresholds != nil && l.deps.Thresholds.Signature() != signature {
				return "", false, errThresholdsChanged
			}
			if err := wake.Write(fr.pcm); err != nil {
				return "", false, err
			}
		case detection, ok := <-detections:
			if !ok {
				if err := wake.Err(); err != nil {
					return "", false, err
				}
				return "", false, nil
			}
			if !l.isListening() {
				return "", false, nil
			}
			return detection.Name, true, nil
		}
	}
}

// dispatchCommand runs a non-wake handler and reports whether it owned the
// detection. The loop keeps listening either way; #81's handler owns the
// volume step, cue and cooldown.
func (l *Loop) dispatchCommand(ctx context.Context, name string) bool {
	command, ok := l.deps.WakeCommands[name]
	if !ok {
		return false
	}
	l.deps.Logger.Info("voice: command", "model", name)
	command(ctx, name)
	return true
}

// isWakeWord applies the explicit allowlist. Empty accepts any name; the
// configured path always has at least one model (config validation refuses an
// empty list while voice input is enabled).
func (l *Loop) isWakeWord(name string) bool {
	if len(l.deps.WakeWords) == 0 {
		return true
	}
	for _, word := range l.deps.WakeWords {
		if word == name {
			return true
		}
	}
	return false
}

// session records one request: chime, listen with endpointing, transcribe, then
// speak. wakeWord is the model that started the turn. It reports whether a wake
// word interrupted the reply (Barge-in) and, when it did, that model's name so
// the caller starts a fresh turn without waiting for another wake word.
func (l *Loop) session(ctx context.Context, wakeWord string) (bool, string, error) {
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
	// Follow-up capture has a deadline only until speech starts.
	followUntil := l.followUntil
	l.followUntil = time.Time{}
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
	// While a Timer or Alarm Ring owns the floor, skip the wake Cue: it would
	// play on top of the ring and the Ring engine never learns about it. The
	// reply still yields to the Ring through the shared speech gate, so capture
	// starts immediately with an empty cue.
	if l.deps.RingActive != nil && l.deps.RingActive() {
		l.deps.Logger.Debug("voice: wake cue skipped while ring active")
		wakePCM, wakeFormat = nil, audio.Format{}
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
		// A busy refusal is a clear, expected answer, not a failure: say so and
		// end the turn instead of silently reopening the wake stream (#368).
		if errors.Is(err, wyoming.ErrBusy) {
			l.speakBusy(sessCtx)
			return false, "", nil
		}
		return false, "", err
	}
	defer stt.Close()

	endpointer, err := NewEndpointer(l.cfg.Endpoint)
	if err != nil {
		return false, "", err
	}

	// Frames captured while the chime played are the chime echoing through the
	// mic. Drop them, including any still buffered when it ends, instead of
	// endpointing them after chimeDone and opening a 15 s utterance on the tail
	// (#18/S6). Frames captured after the chime ends are the user.
	chimeOver := len(wakePCM) == 0
	// Drop queued TTS frames even when the opening Cue is disabled.
	captureAfter := time.Now()
	// Allow the physical speaker/room tail to decay before accepting no-wake audio.
	if !followUntil.IsZero() {
		captureAfter = captureAfter.Add(300 * time.Millisecond)
	}
	var chimeEnd time.Time
	// preroll holds a fixed window of captured frames after the chime filter
	// until the endpointer first reports speech; sttStarted is set once those
	// frames are flushed to STT. The bound never grows with the utterance.
	prerollMax := PrerollFrameCount(l.cfg.FrameBytes, l.cfg.Endpoint.Rate)
	var preroll [][]byte
	sttStarted := false
	for ended := false; !ended; {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case fr := <-l.frames:
			if fr.at.Before(captureAfter) {
				continue
			}
			if !followUntil.IsZero() && !endpointer.SpeechHeard() && time.Now().After(followUntil) {
				return false, "", nil
			}
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
			// After the chime/echo filter: hold a bounded pre-roll until the
			// endpointer first reports speech, so the onset of the phrase
			// reaches STT without streaming the silent pre-utterance of a
			// false wake (#213). The pre-roll starts here, after the chime, so
			// the echo is not replayed.
			ended = endpointer.Add(fr.pcm)
			if sttStarted {
				if err := stt.Write(fr.pcm); err != nil {
					return false, "", err
				}
			} else {
				preroll = append(preroll, append([]byte(nil), fr.pcm...))
				if endpointer.SpeechHeard() {
					// Speech just began: flush the pre-roll oldest-to-newest,
					// including the current frame, so the first word is heard.
					for _, p := range preroll {
						if err := stt.Write(p); err != nil {
							return false, "", err
						}
					}
					preroll = nil
					sttStarted = true
				} else if len(preroll) > prerollMax {
					preroll = preroll[1:]
				}
			}
		case err := <-stt.Refused():
			// The speech-to-text service refused the turn (most often because
			// another session holds the model). A busy refusal is the expected
			// answer: say so and end the turn, without opening the wake stream or
			// streaming the rest of the utterance (#368). Any other refusal is a
			// real failure, so Finish would report it; return it directly.
			if errors.Is(err, wyoming.ErrBusy) {
				l.speakBusy(sessCtx)
				return false, "", nil
			}
			return false, "", err
		}
	}
	// One timeline spans endpointing, STT, routing, the Agent path and
	// playback (#27). It starts at the user's last voiced frame, not the end
	// of the silence window; the same instant is the Filler clock (#25).
	ended := time.Now()
	heard := endpointer.SpeechHeard()
	timeline := latency.New(tool.NewRequestID(), latency.SourceVoice, latency.WithObserver(l.deps.Observer))
	timeline.SetLastVoice(endpointer.LastSpeech())
	if last := endpointer.LastSpeech(); !last.IsZero() {
		timeline.Set(latency.StageEndpointing, ended.Sub(last))
	}
	requestCtx := latency.WithContext(sessCtx, timeline)

	endpointAttrs := []any{
		"request_id", timeline.ID(),
		"speech", heard,
		"silence_ms", endpointer.Silence().Milliseconds(),
	}
	if wakeWord != "" {
		endpointAttrs = append(endpointAttrs, "wake_word", wakeWord)
	}
	if d, ok := timeline.Duration(latency.StageEndpointing); ok {
		endpointAttrs = append(endpointAttrs, "endpointing_ms", d.Milliseconds())
	}
	l.deps.Logger.Info("voice: endpoint", endpointAttrs...)

	// A false wake: the utterance ended on the MaxUtterance cap without any
	// speech. Do not finalise a silent clip (which would pay a 15 s STT
	// finalisation and can hallucinate a transcript); drop the stream via the
	// deferred Close and record the no-audio outcome (#213).
	if !heard {
		l.deps.Logger.Info("voice: no speech", "request_id", timeline.ID(), "wake_word", wakeWord)
		l.deps.Stats.NoSpeech(wakeWord)
		timeline.Complete(l.deps.Logger, latency.OutcomeNoAudio, nil)
		return false, "", nil
	}

	finishStarted := time.Now()
	transcript, err := stt.Finish()
	if err != nil {
		// Same busy refusal can also arrive at Finish when the service only
		// refuses after the model is released; answer clearly rather than
		// failing the turn silently (#368).
		if errors.Is(err, wyoming.ErrBusy) {
			timeline.Complete(l.deps.Logger, latency.OutcomeFailed, err)
			l.speakBusy(sessCtx)
			return false, "", nil
		}
		timeline.Complete(l.deps.Logger, latency.OutcomeFailed, err)
		return false, "", err
	}
	timeline.Set(latency.StageSTTFinalize, time.Since(finishStarted))
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		l.deps.Logger.Info("voice: no speech", "request_id", timeline.ID(), "wake_word", wakeWord)
		l.deps.Stats.NoSpeech(wakeWord)
		timeline.Complete(l.deps.Logger, latency.OutcomeNoAudio, nil)
		return false, "", nil
	}
	if wakeWord != "" {
		timeline.SetAttr("wake_word", wakeWord)
	}
	l.deps.Logger.Info("voice: transcript",
		"request_id", timeline.ID(),
		"wake_word", wakeWord,
		"text", transcript,
		"stt_ms", time.Since(finishStarted).Milliseconds(),
	)

	// Wake detection stays live while the reply plays, so the user can cut in.
	// The stream starts here rather than at the wake word, so the question that
	// just ended cannot fire it.
	watch, detections := l.watch(ctx)
	defer func() {
		if watch != nil {
			_ = watch.Close()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- l.deps.Responder.Respond(requestCtx, Request{Transcript: transcript, Ended: ended, WakeWord: wakeWord})
	}()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return false, "", ctx.Err()
		case err := <-done:
			if err != nil && sessCtx.Err() == nil {
				return false, "", err
			}
			// Playback has drained; keep music ducked by skipping follow-ups when music safety is unknown.
			if l.deps.FollowUp != nil && l.isListening() {
				if window := l.deps.FollowUp(); window > 0 {
					l.followUntil = time.Now().Add(window)
				}
			}
			return false, "", nil
		case fr := <-l.frames:
			if !l.isListening() {
				if watch != nil {
					_ = watch.Close()
					watch, detections = nil, nil
				}
				continue
			}
			if watch == nil {
				continue // no way to interrupt; drain the capture
			}
			if err := watch.Write(fr.pcm); err != nil {
				l.deps.Logger.Warn("voice: barge-in watch", "error", err.Error())
				_ = watch.Close()
				watch, detections = nil, nil
			}
		case detection, ok := <-detections:
			if !ok {
				// The watch stream ended without firing; keep the reply going.
				watch, detections = nil, nil
				continue
			}
			if !l.isListening() {
				if watch != nil {
					_ = watch.Close()
					watch, detections = nil, nil
				}
				continue
			}
			// A command model (#81) acts without interrupting the reply. The
			// stream is single-shot, so reopen it to keep watching.
			if l.dispatchCommand(ctx, detection.Name) {
				_ = watch.Close()
				watch, detections = l.watch(ctx)
				continue
			}
			if !l.isWakeWord(detection.Name) {
				l.deps.Logger.Warn("voice: detection ignored", "model", detection.Name)
				_ = watch.Close()
				watch, detections = l.watch(ctx)
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
			return true, detection.Name, nil
		}
	}
}

// speakBusy tells the user the turn was refused because the speech-to-text
// service is busy with another session. It logs the refusal once and, when a
// Speaker is wired, speaks a short line; without one it stays a log line. The
// turn then ends normally rather than failing silently and reopening the wake
// stream (#368).
func (l *Loop) speakBusy(ctx context.Context) {
	l.deps.Logger.Warn("voice: speech recognition refused the turn as busy", "event", "stt_busy")
	if l.deps.Speaker == nil {
		return
	}
	if err := l.deps.Speaker.SayText(ctx, l.cfg.BusyMessage); err != nil && ctx.Err() == nil {
		l.deps.Logger.Warn("voice: busy message", "error", err.Error())
	}
}

// watch opens the playback-time wake stream. A failure is logged, not fatal:
// the reply still plays, just without the ability to interrupt it.
func (l *Loop) watch(ctx context.Context) (WakeStream, <-chan Detection) {
	if l.deps.Wake == nil || !l.isListening() {
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
		// A sample tap receives real PCM; the normal voice path receives silence.
		l.sampleMu.Lock()
		if l.sampling {
			if l.sampleFrames != nil {
				select {
				case l.sampleFrames <- append([]byte(nil), pcm...):
				default:
					l.sampleOverflow = true
				}
			}
			clear(pcm)
		}
		l.sampleMu.Unlock()
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

// BindFollowUp installs the live window source before Run starts.
func (l *Loop) BindFollowUp(source func() time.Duration) { l.deps.FollowUp = source }

// BindListening installs the live wake-word listening source before Run starts.
func (l *Loop) BindListening(source func() bool) { l.deps.Listening = source }

// isListening reports whether wake detection is enabled. Nil source reports true.
func (l *Loop) isListening() bool {
	if l.deps.Listening == nil {
		return true
	}
	return l.deps.Listening()
}

// drainFrames discards any buffered microphone frames without blocking.
func (l *Loop) drainFrames() {
	if l.frames == nil {
		return
	}
	for {
		select {
		case <-l.frames:
		default:
			return
		}
	}
}
