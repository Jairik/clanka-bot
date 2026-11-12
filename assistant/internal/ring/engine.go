package ring

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

// MaxRamp bounds a configured ramp so a very long ramp cannot outlive the Ring.
const MaxRamp = time.Hour

// Outcome is how a Ring cycle ended.
type Outcome string

const (
	// OutcomeStopped means stop, cancel or snooze ended the Ring.
	OutcomeStopped Outcome = "stopped"
	// OutcomeMissed means max_ring ended the Ring unanswered.
	OutcomeMissed Outcome = "missed"
	// OutcomeInterrupted means the process is shutting down.
	OutcomeInterrupted Outcome = "interrupted"
)

// Request is one alert that wants to Ring. The engine is generic so #53 can
// pass an Alarm with the same shape.
type Request struct {
	Kind Kind
	// ID and Label are the alert's identity for logs and label speech.
	ID    string
	Label string
	// Duration is the original alert duration, used by logs and #62.
	Duration time.Duration
	FiresAt  time.Time
	// FallbackText is spoken once when no Ring sound is assigned. It mirrors
	// the pre-Ring spoken Timer alert so a missing/none sound still alerts.
	FallbackText string
	// Sound optionally replaces the kind's live Cue with one library sound for
	// this alert only (#57). Empty keeps sounds.<kind>_ring. The Alarm manager
	// passes the stored per-Alarm override; a Timer leaves it empty.
	Sound string
	// Volume optionally replaces the kind's Ring volume for this alert only.
	// A nil pointer keeps the live kind setting, so an explicit 0 stays a mute.
	Volume *int
	// Ramp optionally replaces the kind's ramp for this alert only. A nil
	// pointer keeps the live kind setting (30 s for an Alarm), so an explicit
	// zero stays no ramp.
	Ramp *time.Duration
	// OnStart runs when this Ring takes the one speaker slot and claims Focus,
	// before it waits for the current reply sentence. The scheduler uses it to
	// persist the fired -> ringing transition.
	OnStart func()
	// AllowSnooze lets an Alarm handle "snooze" (#53). The engine ends the Ring
	// and reports EndedBy "snooze"; the Alarm scheduler then re-rings after its
	// snooze time. A Timer leaves this false because snooze already acts as
	// stop through the kind rule.
	AllowSnooze bool
	// SnoozeText is what the snooze turn says for an Alarm; empty uses the
	// default "Alarm snoozed.".
	SnoozeText string
}

// Result reports what the Ring did. Played is false only when there was no
// sound and no fallback speech, so the caller can log a dropped alert.
type Result struct {
	Played  bool
	Outcome Outcome
	EndedBy string
}

// Clock is the engine's only time source. Tests use a fake that advances on
// Sleep, so max_ring and gap are deterministic instead of wall-clock sleeps.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// Boundary is the reply-sentence seam. playback.Gate implements it.
type Boundary interface {
	WaitForReplySentence(ctx context.Context) (release func(), err error)
}

// Music is the local duck seam for the later Spotify integration (#72). Duck
// lowers the media volume and returns the restore function the engine always
// calls, on stop, timeout and every error path.
type Music interface {
	Duck(ctx context.Context) (restore func(), err error)
}

// Speaker speaks the label or the no-sound fallback. speech.Speaker satisfies
// it; nil keeps a Ring silent but still timed.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// Deps are the Engine's seams. Settings, Clip and Play are required for sound;
// the rest are optional and stay nil-safe for tests and text-only setups.
type Deps struct {
	// Focus is where the active Ring claims Ring priority. The queue below
	// claims only after it owns the speaker, so a queued Ring cannot replace
	// the Focus of the Ring that is actually sounding.
	Focus *focus.Registry
	// Boundary waits for the current reply sentence. Nil rings immediately.
	Boundary Boundary
	// Music ducks media playback. Nil means there is no music to duck.
	Music Music
	// Speaker speaks the label and the no-sound fallback.
	Speaker Speaker
	// SpeechGain returns the software speech gain percent for this kind. It is
	// read at Ring start; nil means 100. Quiet hours passes the quiet volume
	// for a Timer and 100 for an Alarm, so an Alarm label stays at normal
	// volume even during startup recovery.
	SpeechGain func(Kind) int
	// Settings reads the live per-kind Ring settings at each Ring start.
	Settings func(Kind) Settings
	// Clip resolves the live per-kind Cue sound. False means none assigned.
	Clip func(Kind) (sound.Clip, bool)
	// ClipName resolves one library sound for a per-alert Request.Sound. It is
	// optional: nil or a missing name keeps the kind's Clip. The Alarm module
	// wires the shared sound library; a Timer never sets Request.Sound.
	ClipName func(name string) (sound.Clip, bool)
	// Play writes one rendered pass through the shared sink. It must honour
	// ctx so stop and max_ring can end a pass.
	Play func(ctx context.Context, clip sound.Clip) error
	// Clock is the engine's time source; nil uses the real clock.
	Clock Clock
	// Logger receives lifecycle lines; nil uses the default.
	Logger *slog.Logger
}

// Engine serialises Rings on the one speaker. A second alert waits for the
// first (or the current reply sentence) and only then claims Focus, so every
// Ring stays controllable and no older sound is orphaned by a newer claim.
type Engine struct {
	focus      *focus.Registry
	boundary   Boundary
	music      Music
	speaker    Speaker
	settings   func(Kind) Settings
	speechGain func(Kind) int
	clip       func(Kind) (sound.Clip, bool)
	clipName   func(string) (sound.Clip, bool)
	play       func(context.Context, sound.Clip) error
	clock      Clock
	logger     *slog.Logger

	// slot is the one-speaker lock. A buffered channel gives waiting Rings a
	// stable order and lets their parent context cancel the wait.
	slot chan struct{}
}

// New builds one engine. A nil required seam makes Ring return an unavailable
// result instead of panicking, which keeps headless and test wiring simple.
func New(deps Deps) *Engine {
	if deps.Clock == nil {
		deps.Clock = realClock{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Engine{
		focus:      deps.Focus,
		boundary:   deps.Boundary,
		music:      deps.Music,
		speaker:    deps.Speaker,
		settings:   deps.Settings,
		speechGain: deps.SpeechGain,
		clip:       deps.Clip,
		clipName:   deps.ClipName,
		play:       deps.Play,
		clock:      deps.Clock,
		logger:     deps.Logger,
		slot:       make(chan struct{}, 1),
	}
}

// Ring runs one alert until stop, snooze, max_ring or shutdown. It blocks
// while another Ring owns the speaker, and it claims Focus only once this Ring
// is the one that will sound.
func (e *Engine) Ring(parent context.Context, req Request) Result {
	if e == nil {
		return Result{}
	}
	if parent == nil {
		parent = context.Background()
	}
	select {
	case e.slot <- struct{}{}:
		defer func() { <-e.slot }()
	case <-parent.Done():
		return Result{Outcome: OutcomeInterrupted, EndedBy: "shutdown"}
	}

	// Per-alert overrides are applied to this Ring's copy of the live kind
	// settings. Nothing global is written, so the next alert still uses the
	// kind defaults and an Alarm cannot change another Alarm.
	settings := withOverrides(e.settingsFor(req.Kind), req.Volume, req.Ramp)
	clip, hasClip := e.clipFor(req.Kind)
	if req.Sound != "" {
		if named, ok := e.clipNamed(req.Sound); ok {
			clip, hasClip = named, true
		} else {
			// The library entry vanished or is over its role cap. Keep the
			// kind's sound rather than substituting another named sound.
			e.logger.Warn("ring sound override unavailable; using the kind sound",
				"kind", req.Kind, "alert_id", req.ID, "sound", req.Sound)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	holder := &ringHolder{
		kind:        req.Kind,
		cancel:      cancel,
		allowSnooze: req.AllowSnooze,
		snoozeText:  req.SnoozeText,
	}
	claim := e.claimFocus(holder)
	defer func() {
		// Finish before releasing: a racing intent must decline rather than
		// cancel an already finished Ring.
		holder.finish()
		claim.Release()
	}()
	if req.OnStart != nil {
		req.OnStart()
	}

	// A Ring has priority over the reply, but speech cannot be ducked: wait
	// for the current sentence, not the whole reply. The gate holds the floor
	// from here, so the reply's next sentence waits for this Ring to end.
	release, err := e.waitForSentence(ctx)
	if err != nil {
		return holder.result(parent)
	}
	defer release()

	// Duck music once for the whole Ring; restore on every exit path.
	if restore := e.duck(ctx, req.Kind); restore != nil {
		defer restore()
	}

	if !hasClip || len(clip.PCM) == 0 {
		return e.speakFallback(ctx, parent, req, holder, settings)
	}

	start := e.clock.Now()
	deadline := start.Add(settings.MaxRing)
	firstPass := true
	for {
		if result, done := holder.interrupted(parent); done {
			return result
		}
		remaining := deadline.Sub(e.clock.Now())
		if remaining <= 0 {
			return Result{Played: true, Outcome: OutcomeMissed, EndedBy: "timeout"}
		}

		if shouldSpeakLabel(settings.SpeakLabel, firstPass) {
			if text := labelText(req); text != "" && e.speaker != nil {
				sayCtx, cancelSay := boundedContext(ctx, deadline)
				sayCtx = speech.WithGain(sayCtx, e.speechGainFor(req.Kind))
				err := e.speaker.SayText(sayCtx, text)
				cancelSay()
				if err != nil {
					if result, done := holder.interrupted(parent); done {
						return result
					}
					if e.clock.Now().After(deadline) {
						return Result{Played: true, Outcome: OutcomeMissed, EndedBy: "timeout"}
					}
					e.logger.Warn("ring label", "kind", req.Kind, "error", err.Error())
				}
			}
		}

		elapsed := e.clock.Now().Sub(start)
		pass := scaleClip(clip, settings.Volume, settings.Ramp, elapsed)
		if e.play == nil {
			e.logger.Warn("ring dropped: no playback seam", "kind", req.Kind, "alert_id", req.ID)
			return Result{Outcome: OutcomeStopped, EndedBy: "unavailable"}
		}
		playCtx, cancelPlay := boundedContext(ctx, deadline)
		err := e.play(playCtx, pass)
		cancelPlay()
		if err != nil {
			if result, done := holder.interrupted(parent); done {
				return result
			}
			if e.clock.Now().After(deadline) || errors.Is(err, context.DeadlineExceeded) {
				return Result{Played: true, Outcome: OutcomeMissed, EndedBy: "timeout"}
			}
			// A broken sink would otherwise spin through max_ring. End the
			// Ring so the scheduler records a real outcome.
			e.logger.Error("ring playback", "kind", req.Kind, "alert_id", req.ID, "error", err.Error())
			return Result{Played: true, Outcome: OutcomeStopped, EndedBy: "audio_error"}
		}
		firstPass = false

		remaining = deadline.Sub(e.clock.Now())
		if remaining <= 0 {
			return Result{Played: true, Outcome: OutcomeMissed, EndedBy: "timeout"}
		}
		gap := settings.Gap
		if gap > remaining {
			gap = remaining
		}
		if err := e.clock.Sleep(ctx, gap); err != nil {
			if result, done := holder.interrupted(parent); done {
				return result
			}
			if e.clock.Now().After(deadline) {
				return Result{Played: true, Outcome: OutcomeMissed, EndedBy: "timeout"}
			}
			return Result{Played: true, Outcome: OutcomeStopped, EndedBy: "cancelled"}
		}
	}
}

// speakFallback handles a none/missing sound: the old single spoken line, kept
// inside the same Focus/boundary/max_ring lifecycle so "stop" still reaches it.
func (e *Engine) speakFallback(ctx, parent context.Context, req Request, holder *ringHolder, settings Settings) Result {
	text := req.FallbackText
	if text == "" {
		text = defaultFallback(req.Kind)
	}
	if e.speaker == nil {
		e.logger.Warn("ring dropped: no sound and no speech", "kind", req.Kind, "alert_id", req.ID)
		return Result{Outcome: OutcomeStopped, EndedBy: "unavailable"}
	}
	deadline := e.clock.Now().Add(settings.MaxRing)
	sayCtx, cancel := boundedContext(ctx, deadline)
	sayCtx = speech.WithGain(sayCtx, e.speechGainFor(req.Kind))
	err := e.speaker.SayText(sayCtx, text)
	cancel()
	if err != nil {
		if result, done := holder.interrupted(parent); done {
			return result
		}
		// The fallback is one line; a failed line is not a missed Ring.
		e.logger.Warn("ring fallback speech", "kind", req.Kind, "error", err.Error())
		return Result{Played: true, Outcome: OutcomeStopped, EndedBy: "spoken"}
	}
	if result, done := holder.interrupted(parent); done {
		return result
	}
	return Result{Played: true, Outcome: OutcomeStopped, EndedBy: "spoken"}
}

// settingsFor normalises a possibly partial Settings value.
func (e *Engine) settingsFor(kind Kind) Settings {
	var settings Settings
	if e.settings != nil {
		settings = e.settings(kind)
	} else {
		settings = Defaults(kind)
	}
	if settings.MaxRing <= 0 {
		settings.MaxRing = DefaultMaxRing
	}
	if settings.MaxRing > time.Hour {
		settings.MaxRing = time.Hour
	}
	if settings.Volume < 0 {
		settings.Volume = 0
	}
	if settings.Volume > 100 {
		settings.Volume = 100
	}
	if settings.Ramp < 0 {
		settings.Ramp = 0
	}
	if settings.Ramp > MaxRamp {
		settings.Ramp = MaxRamp
	}
	if settings.Gap < 0 {
		settings.Gap = 0
	}
	if _, ok := ParseSpeakLabel(string(settings.SpeakLabel)); !ok {
		settings.SpeakLabel = SpeakStart
	}
	return settings
}

// withOverrides applies one Request's optional per-alert volume and ramp over
// the kind settings. It clamps the same ranges settingsFor uses, so a corrupt
// stored override cannot mute or ring forever.
func withOverrides(settings Settings, volume *int, ramp *time.Duration) Settings {
	if volume != nil {
		value := *volume
		if value < 0 {
			value = 0
		}
		if value > 100 {
			value = 100
		}
		settings.Volume = value
	}
	if ramp != nil {
		value := *ramp
		if value < 0 {
			value = 0
		}
		if value > MaxRamp {
			value = MaxRamp
		}
		settings.Ramp = value
	}
	return settings
}

// clipFor resolves the live Cue without dereferencing a nil seam.
func (e *Engine) clipFor(kind Kind) (sound.Clip, bool) {
	if e.clip == nil {
		return sound.Clip{}, false
	}
	return e.clip(kind)
}

// clipNamed resolves one per-alert library sound without dereferencing a nil
// seam. A missing seam keeps the kind sound.
func (e *Engine) clipNamed(name string) (sound.Clip, bool) {
	if e.clipName == nil {
		return sound.Clip{}, false
	}
	return e.clipName(name)
}

// waitForSentence uses the reply boundary when one is wired.
func (e *Engine) waitForSentence(ctx context.Context) (func(), error) {
	if e.boundary == nil {
		return func() {}, nil
	}
	return e.boundary.WaitForReplySentence(ctx)
}

// duck lowers music for this Ring and returns the restore function.
func (e *Engine) duck(ctx context.Context, kind Kind) func() {
	if e.music == nil {
		return nil
	}
	restore, err := e.music.Duck(ctx)
	if err != nil {
		e.logger.Warn("ring music duck", "kind", kind, "error", err.Error())
	}
	return restore
}

// speechGainFor clamps the optional label gain. A nil seam means normal volume.
func (e *Engine) speechGainFor(kind Kind) int {
	if e.speechGain == nil {
		return 100
	}
	gain := e.speechGain(kind)
	if gain < 0 {
		return 0
	}
	if gain > 100 {
		return 100
	}
	return gain
}

// claimFocus takes Ring priority, or returns a nil-safe no-op claim when no
// registry is wired.
func (e *Engine) claimFocus(holder focus.Holder) *focus.Claim {
	if e.focus == nil {
		return nil
	}
	return e.focus.Claim(focus.Ring, holder)
}

// shouldSpeakLabel reports whether this pass speaks the label.
func shouldSpeakLabel(mode SpeakLabel, firstPass bool) bool {
	switch mode {
	case SpeakEveryLoop:
		return true
	case SpeakStart:
		return firstPass
	default:
		return false
	}
}

// labelText is the spoken label, or empty when the alert has no label.
func labelText(req Request) string {
	if req.Label == "" {
		return ""
	}
	noun := "timer"
	if req.Kind == Alarm {
		noun = "alarm"
	}
	return capitalize(req.Label) + " " + noun + "."
}

// defaultFallback is used when the caller did not supply the pre-Ring phrase.
func defaultFallback(kind Kind) string {
	if kind == Alarm {
		return "Your alarm is done."
	}
	return "Your timer is done."
}

// capitalize upper-cases the first rune without cutting a multi-byte one.
func capitalize(text string) string {
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	return strings.ToUpper(string(first)) + text[size:]
}

// scaleClip applies the absolute volume and the ramp to one loop pass. The
// ramp is measured from the start of the Ring, so a 30 s ramp spans several
// short passes instead of restarting every loop.
func scaleClip(clip sound.Clip, volume int, ramp, elapsed time.Duration) sound.Clip {
	if len(clip.PCM) == 0 {
		return clip
	}
	if volume >= 100 && ramp <= 0 {
		return clip
	}
	if clip.Format.Width != 2 || clip.Format.Channels != 1 || clip.Format.Rate <= 0 {
		// Cue PCM is always mono PCM16; leave an unexpected layout unchanged
		// rather than reinterpret its bytes.
		return clip
	}
	base := float64(volume) / 100
	samples := len(clip.PCM) / 2
	out := make([]byte, len(clip.PCM))
	for i := 0; i < samples; i++ {
		gain := base
		if ramp > 0 {
			at := elapsed + time.Duration(i)*time.Second/time.Duration(clip.Format.Rate)
			if at < ramp {
				gain *= float64(at) / float64(ramp)
			}
		}
		raw := int16(binary.LittleEndian.Uint16(clip.PCM[i*2:]))
		scaled := float64(raw) * gain
		if scaled > 32767 {
			scaled = 32767
		}
		if scaled < -32768 {
			scaled = -32768
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(scaled)))
	}
	clip.PCM = out
	return clip
}

// boundedContext bounds one speech or playback pass by the Ring deadline. The
// caller cancels it when the pass returns, so a stuck sink cannot outlive
// max_ring.
func boundedContext(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if existing, ok := parent.Deadline(); ok && existing.Before(deadline) {
		return context.WithCancel(parent)
	}
	return context.WithDeadline(parent, deadline)
}

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ringHolder holds Focus at Ring priority for one active Ring and turns stop,
// cancel and snooze into a cancelled Ring. Snooze on a Timer acts as stop; #53
// gives an Alarm its real snooze behaviour.
type ringHolder struct {
	kind        Kind
	cancel      context.CancelFunc
	allowSnooze bool
	snoozeText  string
	done        atomic.Bool

	mu     sync.Mutex
	reason string
}

func (h *ringHolder) Intents() []focus.Intent {
	if h.kind == Timer {
		return []focus.Intent{focus.Stop, focus.Cancel, focus.Snooze}
	}
	if h.allowSnooze {
		return []focus.Intent{focus.Stop, focus.Cancel, focus.Snooze}
	}
	return []focus.Intent{focus.Stop, focus.Cancel}
}

func (h *ringHolder) HandleIntent(_ context.Context, intent focus.Intent) (string, bool) {
	switch intent {
	case focus.Stop, focus.Cancel:
	case focus.Snooze:
		// A Timer has no snooze target; the spec says it acts as stop. An
		// Alarm opts in so #53 can schedule its re-ring after the result.
		if h.kind != Timer && !h.allowSnooze {
			return "", false
		}
	default:
		return "", false
	}
	if !h.done.CompareAndSwap(false, true) {
		return "", false
	}
	h.mu.Lock()
	switch intent {
	case focus.Cancel:
		h.reason = "cancel"
	case focus.Snooze:
		h.reason = "snooze"
	default:
		h.reason = "stop"
	}
	h.mu.Unlock()
	h.cancel()
	if intent == focus.Snooze && h.kind == Alarm {
		if h.snoozeText != "" {
			return h.snoozeText, true
		}
		return "Alarm snoozed.", true
	}
	return stopText(h.kind, intent), true
}

// finish marks the holder done so a racing intent declines.
func (h *ringHolder) finish() { h.done.Store(true) }

// interrupted reports the holder's stop reason when the Ring was cancelled.
func (h *ringHolder) interrupted(parent context.Context) (Result, bool) {
	if h == nil {
		return Result{}, false
	}
	h.mu.Lock()
	reason := h.reason
	h.mu.Unlock()
	if reason != "" {
		return Result{Played: true, Outcome: OutcomeStopped, EndedBy: reason}, true
	}
	if parent != nil && parent.Err() != nil {
		return Result{Played: true, Outcome: OutcomeInterrupted, EndedBy: "shutdown"}, true
	}
	return Result{}, false
}

// result turns a cancelled wait/play into the Ring's outcome.
func (h *ringHolder) result(parent context.Context) Result {
	if result, done := h.interrupted(parent); done {
		return result
	}
	return Result{Outcome: OutcomeInterrupted, EndedBy: "shutdown"}
}

// stopText is what the stop turn says for one kind and intent.
func stopText(kind Kind, intent focus.Intent) string {
	if kind == Alarm {
		if intent == focus.Cancel {
			return "Alarm cancelled."
		}
		return "Alarm stopped."
	}
	if intent == focus.Cancel {
		return "Timer cancelled."
	}
	return "Timer stopped."
}
