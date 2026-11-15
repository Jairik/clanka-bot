// Package voicecmd handles the wake-free command models: detections such as
// "volume up" that the wake word service reports by name and that the voice
// loop dispatches without opening STT or starting a turn (#81).
//
// A command acts only while music or the current reply holds Focus, takes one
// bounded volume step on the sink that audio.volume_set drives, and plays a
// short Cue. A cooldown between accepted detections keeps one misheard word
// from walking the volume to zero.
//
// The target is the shared Pulse sink, not a per-stream gain. That is the knob
// audio.volume_set and the Dashboard slider drive, and it carries replies, Cues
// and music together. Quiet hours and the Ring engine apply their own volumes
// in software to PCM, so they scale with this sink but are never rewritten
// here; the command does not touch quiet.volume or ring.*. music.max_volume is
// Spotify's own cap, but because the sink also carries music this handler uses
// it as a ceiling on an up step while music holds Focus; a down step is never
// blocked, and an up step above the cap is a no-op rather than a drop.
//
// Handle runs on the voice loop's reading goroutine, so it never waits for the
// sink: the in-memory gate (Focus, cooldown, one command in flight) decides
// there, and a slow pactl runs on the command's own goroutine while the wake
// stream keeps being read (#209).
package voicecmd

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// Model names this package knows how to run. They are also the openWakeWord
// model ids the service is asked for when commands are enabled.
const (
	VolumeUpModel   = "volume_up"
	VolumeDownModel = "volume_down"
)

// DefaultSinkTimeout bounds one sink call when Deps.SinkTimeout is unset. It
// matches the Dashboard's volume timeout.
const DefaultSinkTimeout = 3 * time.Second

// Sink is the volume seam audio.volume_set uses in production. It is a
// pointer-free interface so tests can supply a fake without Pulse.
type Sink interface {
	Volume(ctx context.Context) (audio.VolumeState, error)
	SetVolume(ctx context.Context, percent int) error
}

// Config is the fixed shape of one handler.
type Config struct {
	// Step is the volume change per detection, in percent. 1..50.
	Step int
	// Cooldown is the minimum time between accepted detections.
	Cooldown time.Duration
}

// Deps are the live collaborators. Focus and MaxVolume are read per detection,
// so a Focus claim, a release or a Dashboard cap change applies immediately.
type Deps struct {
	Sink Sink
	// Focus reports whether the current reply and playing music hold Focus.
	// Both can be true at once: music is ducked during a reply and keeps its
	// claim until playback stops.
	Focus func() (reply, music bool)
	// MaxVolume is the live music.max_volume cap, 0 when unset. It is applied
	// while music holds Focus, because the sink is shared by music and speech.
	MaxVolume func() int
	// Cue acknowledges an accepted command. It runs on the command's own
	// goroutine, never on the voice loop's reading goroutine, so it may take
	// as long as a sound needs; it still should not block indefinitely.
	Cue func(ctx context.Context)
	// Now is the clock; tests advance it. Nil uses time.Now.
	Now func() time.Time
	// SinkTimeout bounds each Volume/SetVolume call. Handle returns before the
	// sink is touched, so the bound protects only the command's own goroutine:
	// it keeps a wedged pactl from holding the one-command-in-flight slot.
	// Zero uses DefaultSinkTimeout.
	SinkTimeout time.Duration
	// Logger is optional. Ignored detections log at debug level so a healthy
	// run does not grow the log with every phrase the room happens to say.
	Logger *slog.Logger
}

// Handler runs the command detections. It is safe for concurrent use, though
// the voice loop calls Handle serially. The zero value is not usable.
type Handler struct {
	cfg  Config
	deps Deps

	mu sync.Mutex
	// last is when the last command was accepted, for the cooldown.
	last time.Time
	// done is non-nil while a command runs and is closed when it finishes, so
	// Wait can join it without a second counter.
	done chan struct{}
}

// New validates a handler. Step and cooldown come from wakeword.commands.
func New(cfg Config, deps Deps) (*Handler, error) {
	if deps.Sink == nil {
		return nil, errors.New("voicecmd: sink is required")
	}
	if deps.Focus == nil {
		return nil, errors.New("voicecmd: focus is required")
	}
	if cfg.Step < 1 || cfg.Step > 50 {
		return nil, errors.New("voicecmd: step must be between 1 and 50")
	}
	if cfg.Cooldown <= 0 {
		return nil, errors.New("voicecmd: cooldown must be positive")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.SinkTimeout <= 0 {
		deps.SinkTimeout = DefaultSinkTimeout
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{cfg: cfg, deps: deps}, nil
}

// Handle is the voice.WakeCommand for both volume models. It runs on the voice
// loop's reading goroutine and returns before the sink is touched: the checks
// it keeps here (model name, Focus, cooldown, one command in flight) are all
// in-memory, and the volume read, write and Cue run on the command's own
// goroutine. A slow or wedged pactl therefore cannot stop the wake stream from
// being read (#209).
//
// One detection is one command. A second detection while a command is still
// running is dropped rather than queued, because the burst that produces it is
// a misheard word, not a request for two steps.
func (h *Handler) Handle(ctx context.Context, name string) {
	delta, ok := deltaFor(name, h.cfg.Step)
	if !ok {
		h.deps.Logger.Debug("voicecmd: unknown model", "model", name)
		return
	}

	reply, music := h.deps.Focus()
	if !reply && !music {
		h.deps.Logger.Debug("voicecmd: ignored", "model", name, "reason", "nothing holds focus")
		return
	}

	done, ok := h.begin(name)
	if !ok {
		return
	}
	go func() {
		defer h.end(done)
		h.run(ctx, name, delta)
	}()
}

// Wait blocks until the command that was in flight when it was called has
// finished. The voice loop never needs it — Handle has already returned — but
// shutdown uses it so a volume write is not abandoned, and tests use it to
// join the goroutine.
func (h *Handler) Wait() {
	h.mu.Lock()
	done := h.done
	h.mu.Unlock()
	if done != nil {
		<-done
	}
}

// begin applies the cooldown and the one-command-in-flight rule, and records
// the acceptance. Both checks are in-memory, so a dropped detection costs the
// loop goroutine nothing but the log line.
func (h *Handler) begin(name string) (chan struct{}, bool) {
	now := h.deps.Now()
	h.mu.Lock()
	if h.done != nil {
		h.mu.Unlock()
		h.deps.Logger.Debug("voicecmd: ignored", "model", name, "reason", "command in flight")
		return nil, false
	}
	if !h.last.IsZero() && now.Sub(h.last) < h.cfg.Cooldown {
		elapsed := now.Sub(h.last)
		h.mu.Unlock()
		h.deps.Logger.Debug("voicecmd: ignored", "model", name, "reason", "cooldown", "elapsed_ms", elapsed.Milliseconds())
		return nil, false
	}
	done := make(chan struct{})
	h.done = done
	h.last = now
	h.mu.Unlock()
	return done, true
}

// end releases the in-flight slot before the waiter is woken, so a Wait that
// returns can be followed immediately by another Handle.
func (h *Handler) end(done chan struct{}) {
	h.mu.Lock()
	h.done = nil
	h.mu.Unlock()
	close(done)
}

// run performs one accepted command off the loop goroutine. Focus is read
// again here, so a claim released between the detection and the sink call is
// seen at execution time and the volume is left alone.
func (h *Handler) run(ctx context.Context, name string, delta int) {
	reply, music := h.deps.Focus()
	if !reply && !music {
		h.deps.Logger.Debug("voicecmd: ignored", "model", name, "reason", "focus released")
		return
	}

	sinkCtx, cancel := context.WithTimeout(ctx, h.deps.SinkTimeout)
	defer cancel()
	state, err := h.deps.Sink.Volume(sinkCtx)
	if err != nil {
		h.deps.Logger.Warn("voicecmd: read volume", "model", name, "error", err.Error())
		return
	}
	target, capped := h.target(state.Percent, delta, music)
	if target == state.Percent {
		h.deps.Logger.Debug("voicecmd: no change", "model", name, "volume", state.Percent, "capped", capped)
		return
	}
	if err := h.deps.Sink.SetVolume(sinkCtx, target); err != nil {
		h.deps.Logger.Warn("voicecmd: set volume", "model", name, "error", err.Error())
		return
	}
	h.deps.Logger.Info("voicecmd: volume",
		"model", name,
		"from", state.Percent,
		"to", target,
		"delta", target-state.Percent,
		"music", music,
		"reply", reply,
		"capped", capped,
	)
	if h.deps.Cue != nil {
		h.deps.Cue(ctx)
	}
}

// target applies one step, the 100% ceiling and the music cap. The music cap
// is a ceiling only: a down step never stops above it, so the user can always
// make the room quieter. An up step above the 100% ceiling is a no-op instead
// of a clamp, so a level set above 100 elsewhere is never lowered by "louder".
func (h *Handler) target(current, delta int, music bool) (int, bool) {
	if delta > 0 && current >= 100 {
		return current, true
	}
	target := current + delta
	if target < 0 {
		target = 0
	}
	if target > 100 {
		target = 100
	}
	if music && delta > 0 && h.deps.MaxVolume != nil {
		if limit := h.deps.MaxVolume(); limit > 0 && target > limit {
			if current >= limit {
				// Already at or above the cap. An up step must never lower the
				// volume just because the cap sits below the current level.
				return current, true
			}
			return limit, true
		}
	}
	return target, false
}

// deltaFor maps a model to its direction. A name this package does not own is
// reported false so the caller can log it without guessing.
func deltaFor(name string, step int) (int, bool) {
	switch name {
	case VolumeUpModel:
		return step, true
	case VolumeDownModel:
		return -step, true
	default:
		return 0, false
	}
}
