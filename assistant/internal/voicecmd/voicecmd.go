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
	// Cue acknowledges an accepted command. It must not block: the voice loop
	// calls Handle on its reading goroutine. A silent Cue (nil) is valid.
	Cue func(ctx context.Context)
	// Now is the clock; tests advance it. Nil uses time.Now.
	Now func() time.Time
	// SinkTimeout bounds each Volume/SetVolume call. Handle runs on the voice
	// loop's reading goroutine, so a wedged pactl must not stop the wake
	// stream from being fed. Zero uses DefaultSinkTimeout.
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

	mu   sync.Mutex
	last time.Time
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

// Handle is the voice.WakeCommand for both volume models. It returns without
// waiting for the Cue, so the wake stream stays fed.
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

	h.mu.Lock()
	now := h.deps.Now()
	if !h.last.IsZero() && now.Sub(h.last) < h.cfg.Cooldown {
		elapsed := now.Sub(h.last)
		h.mu.Unlock()
		h.deps.Logger.Debug("voicecmd: ignored", "model", name, "reason", "cooldown", "elapsed_ms", elapsed.Milliseconds())
		return
	}
	h.last = now
	h.mu.Unlock()

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

// target applies one step and the music cap. The cap is a ceiling only: a down
// step never stops above it, so the user can always make the room quieter.
func (h *Handler) target(current, delta int, music bool) (int, bool) {
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
