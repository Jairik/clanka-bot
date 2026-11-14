package alarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// NewSet builds alarm.set.
func NewSet(m *Manager) tool.Tool[SetIn, SetOut] {
	return tool.Tool[SetIn, SetOut]{
		Name:        SetName,
		Description: "Set a clock-time alarm, optionally repeating on chosen days and with a per-alarm sound, volume or ramp. Completion audio replaces speech: use a song, Spotify link, playlist: name (shuffled), or YouTube video link; play it only at the alarm time.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in SetIn) (SetOut, error) {
			return m.Set(ctx, in)
		},
		Speak: func(out SetOut) string {
			clock := formatClock(out.Hour, out.Minute)
			repeat := describeSchedule(scheduleSpec{schedule: out.Schedule, days: weekdayMaskOf(out.Weekdays)}, out.Weekdays)
			text := "Alarm set for " + clock + repeat
			if out.Label != "" {
				text = capitalize(out.Label) + " alarm set for " + clock + repeat
			}
			text += describeOverrides(out.Alarm)
			if out.SoundFallback {
				text += ", but I couldn't find that sound, so it uses the default sound"
			}
			return text + "."
		},
		Examples: []string{
			"wake me at 7",
			"wake me up at 6:30",
			"set an alarm for 7 in the morning",
			"set an alarm for 6:30 on weekdays",
			"set an alarm for 8 on weekends",
			"set an alarm for 7 every day",
			"set an alarm for 6:45 on monday",
			"set a work alarm for 7:30",
		},
		Extract: extractSet,
	}
}

// NewList builds alarm.list.
func NewList(m *Manager) tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the configured alarms and when the next one rings.",
		Permission:  tool.PermissionRead,
		// The list names private labels, so it never leaves the Pi.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, ListIn) (ListOut, error) {
			return ListOut{Alarms: m.List()}, nil
		},
		Speak: func(out ListOut) string {
			switch len(out.Alarms) {
			case 0:
				return "No alarms are set."
			case 1:
				return describeAlarm(out.Alarms[0])
			default:
				next := out.Alarms[0]
				return fmt.Sprintf("You have %d alarms. The next is at %s.",
					len(out.Alarms), formatClock(next.Hour, next.Minute))
			}
		},
		Examples: []string{
			"what alarms do I have",
			"list my alarms",
			"show me my alarms",
			"what alarms are set",
			"do I have any alarms",
			"when is my next alarm",
		},
	}
}

// NewCancel builds alarm.cancel.
func NewCancel(m *Manager) tool.Tool[CancelIn, CancelOut] {
	return tool.Tool[CancelIn, CancelOut]{
		Name:        CancelName,
		Description: "Cancel an alarm by time, label or id; no argument cancels every alarm.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in CancelIn) (CancelOut, error) {
			cancelled, err := m.Cancel(ctx, in)
			return CancelOut{Cancelled: cancelled}, err
		},
		Speak: func(out CancelOut) string {
			switch out.Cancelled {
			case 0:
				return "No alarm to cancel."
			case 1:
				return "Alarm cancelled."
			default:
				return fmt.Sprintf("Cancelled %d alarms.", out.Cancelled)
			}
		},
		Examples: []string{
			"cancel my 7 am alarm",
			"cancel the alarm",
			"cancel my alarm",
			"delete my alarm",
			"remove the 6:30 alarm",
			"cancel my work alarm",
			"cancel my wake up alarm",
		},
		Extract: extractCancel,
	}
}

// NewUpdate builds alarm.update. It changes only the per-Alarm overrides; the
// schedule, label and next occurrence stay as they are.
func NewUpdate(m *Manager) tool.Tool[UpdateIn, UpdateOut] {
	return tool.Tool[UpdateIn, UpdateOut]{
		Name:        UpdateName,
		Description: "Change an existing alarm's sound, volume or ramp; time, label and repeat are unchanged.",
		Permission:  tool.PermissionAct,
		// Like alarm.set/cancel, it changes only the local schedule and the
		// reply names the target the user already supplied. It never lists
		// other alarms, which is what keeps alarm.list LocalOnlyYes.
		LocalOnly: tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in UpdateIn) (UpdateOut, error) {
			return m.Update(ctx, in)
		},
		Speak: speakUpdate,
		Examples: []string{
			"change my 7 am alarm to the birds sound",
			"change my 7 am alarm to birds",
			"change my alarm sound to the birds sound",
			"change my alarm's sound to birds",
			"update my 7 am alarm to the birds sound",
			"switch my 7 am alarm to the birds sound",
			"make my 7 am alarm use the birds sound",
			"use the birds sound for my 7 am alarm",
			"set my alarm sound to birds",
			"set my alarm volume to 40",
			"update my alarm volume to 30",
			"change my alarm volume to 40",
			"change my alarm ramp to 10 seconds",
			"change my 7 am alarm ramp to 5 seconds",
			"clear my alarm sound",
			"reset my alarm volume",
			"use the default sound for my alarm",
		},
		Extract: extractUpdate,
	}
}

// Register adds every alarm Tool.
func Register(r *tool.Registry, m *Manager) error {
	if err := tool.Add(r, NewSet(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewList(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewCancel(m)); err != nil {
		return err
	}
	return tool.Add(r, NewUpdate(m))
}

// extractSet reads the clock time, repeat rule and optional per-Alarm
// overrides from a spoken alarm request. It requires an alarm or wake phrase,
// so a misrouted timer request cannot create an Alarm, and it refuses wording
// that asks to change an existing Alarm.
func extractSet(text string) (SetIn, bool) {
	text, media := ring.SplitAudio(text)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "alarm") && !strings.Contains(lower, "wake") {
		return SetIn{}, false
	}
	if looksLikeAlarmUpdate(lower) {
		return SetIn{}, false
	}
	// Strip the override clauses before reading the clock, so a ramp such as
	// "10 seconds" cannot become an Alarm time.
	hour, minute, ok := parse.Clock(stripOverridePhrases(text))
	if !ok {
		return SetIn{}, false
	}
	schedule, weekdays := parseScheduleText(text)
	overrides := parseOverrides(text)
	return SetIn{
		Audio:       media,
		Hour:        hour,
		Minute:      minute,
		Schedule:    string(schedule),
		Weekdays:    weekdays,
		Label:       labelForAlarm(text),
		Tomorrow:    mentionsTomorrow(text),
		Sound:       overrides.Sound,
		Volume:      overrides.Volume,
		RampSeconds: overrides.RampSeconds,
	}, true
}

// extractUpdate reads the selector and the per-Alarm override change. It
// requires an alarm word, an override phrase and an update cue, so "wake me at
// 7 with the birds sound" stays with alarm.set.
func extractUpdate(text string) (UpdateIn, bool) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "alarm") {
		return UpdateIn{}, false
	}
	if containsWord(lower, "wake") {
		return UpdateIn{}, false
	}
	overrides := parseOverrides(text)
	if !overrides.hasChanges() {
		return UpdateIn{}, false
	}
	if !looksLikeAlarmUpdate(lower) {
		return UpdateIn{}, false
	}
	in := UpdateIn{
		Sound:       overrides.Sound,
		SoundReset:  overrides.SoundReset,
		Volume:      overrides.Volume,
		VolumeReset: overrides.VolumeReset,
		RampSeconds: overrides.RampSeconds,
		RampReset:   overrides.RampReset,
	}
	// The selector clock is read after the override clauses are removed, so
	// "change my alarm volume to 10" does not look for a 10:00 Alarm.
	if hour, minute, ok := parse.Clock(stripOverridePhrases(text)); ok {
		in.Time = fmt.Sprintf("%02d:%02d", hour, minute)
	}
	in.Label = labelForAlarm(stripOverridePhrases(text))
	return in, true
}

// describeOverrides renders the stored per-Alarm overrides for a set reply.
func describeOverrides(alarm Alarm) string {
	text := ""
	if alarm.Sound != "" {
		text += " with the " + alarm.Sound + " sound"
	}
	if alarm.Volume != nil {
		text += fmt.Sprintf(" at %d%% volume", *alarm.Volume)
	}
	if alarm.RampSeconds != nil {
		text += " with " + rampPhrase(*alarm.RampSeconds)
	}
	return text
}

// speakUpdate says exactly what changed on the matched Alarm.
func speakUpdate(out UpdateOut) string {
	if out.Ambiguous {
		return "More than one alarm matches. Which one should I change?"
	}
	if out.Matched == 0 {
		return "No alarm matched."
	}
	target := "your " + formatClock(out.Alarm.Hour, out.Alarm.Minute) + " alarm"
	if out.Alarm.Label != "" {
		target = "your " + out.Alarm.Label + " alarm"
	}
	var parts []string
	for _, change := range out.Changed {
		switch change {
		case "sound":
			if out.SoundFallback {
				continue // named in the fallback sentence below
			}
			if out.Alarm.Sound == "" {
				parts = append(parts, "the default sound")
			} else {
				parts = append(parts, "the "+out.Alarm.Sound+" sound")
			}
		case "volume":
			if out.Alarm.Volume == nil {
				parts = append(parts, "the default volume")
			} else {
				parts = append(parts, fmt.Sprintf("volume %d", *out.Alarm.Volume))
			}
		case "ramp":
			if out.Alarm.RampSeconds == nil {
				parts = append(parts, "the default ramp")
			} else {
				parts = append(parts, rampPhrase(*out.Alarm.RampSeconds))
			}
		}
	}
	if out.SoundFallback {
		prefix := "Changed " + target
		if len(parts) > 0 {
			prefix += ": " + joinPhrases(parts)
		}
		return prefix + ", but I couldn't find that sound, so it uses the default sound."
	}
	switch len(parts) {
	case 0:
		return "Nothing changed on " + target + "."
	case 1:
		return "Changed " + target + " to " + parts[0] + "."
	default:
		return "Changed " + target + ": " + joinPhrases(parts) + "."
	}
}

// rampPhrase renders a whole-second ramp for a reply.
func rampPhrase(seconds int) string {
	switch {
	case seconds <= 0:
		return "no ramp"
	case seconds < 60:
		return fmt.Sprintf("a %d second ramp", seconds)
	case seconds%60 == 0:
		return fmt.Sprintf("a %d minute ramp", seconds/60)
	default:
		return fmt.Sprintf("a %d minute %d second ramp", seconds/60, seconds%60)
	}
}

// joinPhrases renders "a, b and c".
func joinPhrases(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// extractCancel reads the alarm selector. A request that does not mention an
// alarm is not an alarm.cancel request, so a misrouted "cancel my subscription"
// cannot cancel every Alarm. A request about a sound, volume or ramp override
// is not a cancel either, so "remove the sound from my alarm" cannot delete it.
func extractCancel(text string) (CancelIn, bool) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "alarm") {
		return CancelIn{}, false
	}
	if looksLikeAlarmUpdate(lower) {
		return CancelIn{}, false
	}
	in := CancelIn{Label: labelForAlarm(text)}
	if hour, minute, ok := parse.Clock(text); ok {
		in.Time = fmt.Sprintf("%02d:%02d", hour, minute)
	}
	return in, true
}

// mentionsTomorrow reports whether the request names tomorrow explicitly.
func mentionsTomorrow(text string) bool {
	for _, word := range wordsOf(text) {
		if word == "tomorrow" {
			return true
		}
	}
	return false
}

// weekdayMaskOf turns spoken weekday names into the stored bitmask.
func weekdayMaskOf(names []string) uint8 {
	mask, _, err := parseWeekdayList(names)
	if err != nil {
		return 0
	}
	return mask
}

// describeAlarm renders one Alarm for the list reply.
func describeAlarm(alarm Alarm) string {
	repeat := describeSchedule(scheduleSpec{schedule: alarm.Schedule, days: weekdayMaskOf(alarm.Weekdays)}, alarm.Weekdays)
	clock := formatClock(alarm.Hour, alarm.Minute)
	if alarm.Label != "" {
		return capitalize(alarm.Label) + " alarm is set for " + clock + repeat + "."
	}
	return "Your alarm is set for " + clock + repeat + "."
}
