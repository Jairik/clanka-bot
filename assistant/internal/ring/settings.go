// Package ring owns the Ring: the repeating sound a Timer or Alarm makes when
// it fires, until stop, snooze or max_ring. The engine is shared by both kinds
// and reads its per-kind settings live, so #53 reuses it without another
// settings or audio path.
package ring

import (
	"fmt"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Kind is the Scheduled-alert kind a Ring belongs to. The per-kind settings use
// this string, so adding Alarm does not change the Timer behaviour.
type Kind string

const (
	// Timer rings a duration alert.
	Timer Kind = "timer"
	// Alarm rings a clock-time alert (#53).
	Alarm Kind = "alarm"
)

// SpeakLabel is the label speech mode.
type SpeakLabel string

const (
	// SpeakStart speaks the label once at the start of the Ring.
	SpeakStart SpeakLabel = "start"
	// SpeakEveryLoop speaks the label before every loop pass.
	SpeakEveryLoop SpeakLabel = "every_loop"
	// SpeakNever speaks no label.
	SpeakNever SpeakLabel = "never"
)

const (
	// DefaultMaxRing is the default time a Ring sounds before it is missed.
	DefaultMaxRing = 5 * time.Minute
	// DefaultVolume is the default absolute Ring volume percent. It is applied
	// in software to the Cue PCM and is independent of the media volume.
	DefaultVolume = 80
	// DefaultGap is the default silence between loop passes.
	DefaultGap = time.Second
	// DefaultAlarmRamp is the default Alarm ramp, so it wakes gently.
	DefaultAlarmRamp = 30 * time.Second
	// DefaultAlarmSnooze is the default re-ring delay after "snooze" on an
	// Alarm. A Timer's snooze acts as stop, so only Alarm uses it.
	DefaultAlarmSnooze = 9 * time.Minute
)

// Settings are one kind's live Ring settings. Sound is not a field: the Cue
// role key (SoundKey) already names the assigned sound for the kind. Snooze is
// read by the Alarm scheduler after a snooze outcome; the Ring engine itself
// only ends the Ring and reports the reason.
type Settings struct {
	MaxRing    time.Duration
	Volume     int
	Ramp       time.Duration
	Gap        time.Duration
	SpeakLabel SpeakLabel
	Snooze     time.Duration
}

// Defaults returns the shipped Ring settings for one kind. Timer ramp is 0 s
// and has no snooze; Alarm ramp is 30 s and snoozes for 9 min. An unknown kind
// is treated as a Timer.
func Defaults(kind Kind) Settings {
	ramp := time.Duration(0)
	snooze := time.Duration(0)
	if kind == Alarm {
		ramp = DefaultAlarmRamp
		snooze = DefaultAlarmSnooze
	}
	return Settings{
		MaxRing:    DefaultMaxRing,
		Volume:     DefaultVolume,
		Ramp:       ramp,
		Gap:        DefaultGap,
		SpeakLabel: SpeakStart,
		Snooze:     snooze,
	}
}

// ValidKind reports whether kind is Timer or Alarm.
func ValidKind(kind Kind) bool { return kind == Timer || kind == Alarm }

// SoundKey is the live Cue role setting that selects the Ring sound for kind.
// #55 created sounds.timer_ring and sounds.alarm_ring, so Ring does not add a
// second sound key that could disagree with the Sounds page.
func SoundKey(kind Kind) string {
	if kind == Alarm {
		return "sounds.alarm_ring"
	}
	return "sounds.timer_ring"
}

// Key is one live Ring setting key, for example ring.timer.max_ring.
func Key(kind Kind, field string) string {
	return "ring." + string(kind) + "." + field
}

// SettingsKeys returns every key a Ring reads, sound role included. Alarm
// adds the snooze delay the Alarm scheduler reads after a snooze outcome.
func SettingsKeys(kind Kind) []string {
	keys := []string{
		SoundKey(kind),
		Key(kind, "max_ring"),
	}
	if kind == Alarm {
		keys = append(keys, Key(kind, "snooze"))
	}
	return append(keys,
		Key(kind, "volume"),
		Key(kind, "ramp"),
		Key(kind, "gap"),
		Key(kind, "speak_label"),
	)
}

// Reader is the read side of the settings service. *settings.Service
// satisfies it; tests can use the real service over a temporary database.
type Reader interface {
	Get(key string) (settings.Value, bool)
}

// ReadSettings resolves one kind's settings from the live service. Missing or
// mistyped values fall back to Defaults, so a partially registered service
// cannot mute a Ring or run it forever.
func ReadSettings(svc Reader, kind Kind) Settings {
	out := Defaults(kind)
	if svc == nil {
		return out
	}
	if value, ok := svc.Get(Key(kind, "max_ring")); ok {
		if d, ok := value.Value.(time.Duration); ok && d > 0 {
			out.MaxRing = d
		}
	}
	if value, ok := svc.Get(Key(kind, "volume")); ok {
		if n, ok := value.Value.(int); ok && n >= 0 && n <= 100 {
			out.Volume = n
		}
	}
	if value, ok := svc.Get(Key(kind, "ramp")); ok {
		if d, ok := value.Value.(time.Duration); ok && d >= 0 {
			out.Ramp = d
		}
	}
	if value, ok := svc.Get(Key(kind, "gap")); ok {
		if d, ok := value.Value.(time.Duration); ok && d >= 0 {
			out.Gap = d
		}
	}
	if value, ok := svc.Get(Key(kind, "speak_label")); ok {
		if text, ok := value.Value.(string); ok {
			if mode, ok := ParseSpeakLabel(text); ok {
				out.SpeakLabel = mode
			}
		}
	}
	if kind == Alarm {
		if value, ok := svc.Get(Key(kind, "snooze")); ok {
			if d, ok := value.Value.(time.Duration); ok && d > 0 {
				out.Snooze = d
			}
		}
	}
	return out
}

// Definitions builds both kinds' live Ring settings. They are ordinary Live
// settings, so #48's generic Dashboard page shows and edits them without a
// page-local list.
func Definitions() []settings.Definition {
	var defs []settings.Definition
	for _, kind := range []Kind{Timer, Alarm} {
		kind := kind
		defaults := Defaults(kind)
		defs = append(defs,
			settings.Duration(Key(kind, "max_ring"), defaults.MaxRing,
				settings.WithDescription(fmt.Sprintf(
					"How long a %s Ring sounds before the alert is recorded as missed. Default %s.",
					kind, defaults.MaxRing)),
				settings.WithValidator(validMaxRing(kind)),
			),
			settings.Int(Key(kind, "volume"), defaults.Volume,
				settings.WithDescription(fmt.Sprintf(
					"Absolute %s Ring volume percent, applied to the Cue and independent of the media volume.",
					kind)),
				settings.WithValidator(validVolume(kind)),
			),
			settings.Duration(Key(kind, "ramp"), defaults.Ramp,
				settings.WithDescription(fmt.Sprintf(
					"Time over which the %s Ring rises from silence to its volume. Default %s.",
					kind, defaults.Ramp)),
				settings.WithValidator(validDuration(kind, "ramp")),
			),
			settings.Duration(Key(kind, "gap"), defaults.Gap,
				settings.WithDescription(fmt.Sprintf(
					"Silence between %s Ring loops. Default %s.", kind, defaults.Gap)),
				settings.WithValidator(validDuration(kind, "gap")),
			),
			settings.String(Key(kind, "speak_label"), string(defaults.SpeakLabel),
				settings.WithDescription(fmt.Sprintf(
					"When the %s label is spoken: start, every_loop or never. Default %s.",
					kind, defaults.SpeakLabel)),
				settings.WithValidator(validSpeakLabel(kind)),
			),
		)
		if kind == Alarm {
			// The Alarm scheduler reads this after a snooze outcome; the Ring
			// engine itself does not use it, so a Timer has no snooze key.
			defs = append(defs, settings.Duration(Key(kind, "snooze"), defaults.Snooze,
				settings.WithDescription(fmt.Sprintf(
					"How long a snoozed Alarm waits before it Rings again. Default %s.",
					defaults.Snooze)),
				settings.WithValidator(validSnooze(kind)),
			))
		}
	}
	return defs
}

// ParseSpeakLabel normalises and validates one label speech mode.
func ParseSpeakLabel(text string) (SpeakLabel, bool) {
	switch SpeakLabel(strings.ToLower(strings.TrimSpace(text))) {
	case SpeakStart:
		return SpeakStart, true
	case SpeakEveryLoop:
		return SpeakEveryLoop, true
	case SpeakNever:
		return SpeakNever, true
	default:
		return "", false
	}
}

// validMaxRing requires a positive, bounded Ring so a bad setting cannot make
// a Timer ring forever or time out before it speaks.
func validMaxRing(kind Kind) func(any) error {
	return func(value any) error {
		d, ok := value.(time.Duration)
		if !ok {
			return fmt.Errorf("ring: %s max_ring must be a duration", kind)
		}
		if d <= 0 || d > time.Hour {
			return fmt.Errorf("ring: %s max_ring must be between 1s and 1h", kind)
		}
		return nil
	}
}

// validVolume allows the full audible range, including an explicit mute.
func validVolume(kind Kind) func(any) error {
	return func(value any) error {
		n, ok := value.(int)
		if !ok {
			return fmt.Errorf("ring: %s volume must be an integer percent", kind)
		}
		if n < 0 || n > 100 {
			return fmt.Errorf("ring: %s volume must be between 0 and 100", kind)
		}
		return nil
	}
}

// validDuration requires a non-negative duration. Ramp and gap may be zero.
func validDuration(kind Kind, field string) func(any) error {
	return func(value any) error {
		d, ok := value.(time.Duration)
		if !ok {
			return fmt.Errorf("ring: %s %s must be a duration", kind, field)
		}
		if d < 0 {
			return fmt.Errorf("ring: %s %s must not be negative", kind, field)
		}
		return nil
	}
}

// validSpeakLabel rejects a typo rather than silently choosing a mode.
func validSpeakLabel(kind Kind) func(any) error {
	return func(value any) error {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("ring: %s speak_label must be a string", kind)
		}
		if _, ok := ParseSpeakLabel(text); !ok {
			return fmt.Errorf("ring: %s speak_label must be start, every_loop or never", kind)
		}
		return nil
	}
}

// validSnooze bounds the Alarm re-ring delay. A short delay is still useful
// for a quick repeat; an hour is the ceiling so a snooze cannot overlap the
// next daily occurrence.
func validSnooze(kind Kind) func(any) error {
	return func(value any) error {
		d, ok := value.(time.Duration)
		if !ok {
			return fmt.Errorf("ring: %s snooze must be a duration", kind)
		}
		if d < time.Second || d > time.Hour {
			return fmt.Errorf("ring: %s snooze must be between 1s and 1h", kind)
		}
		return nil
	}
}
