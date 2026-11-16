package alarm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

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
			ringing, ringingIndex := 0, -1
			for i, alarm := range out.Alarms {
				if alarm.State != StateRinging {
					continue
				}
				ringing++
				ringingIndex = i
			}
			switch len(out.Alarms) {
			case 0:
				return "No alarms are set."
			case 1:
				if ringing == 1 {
					return ringingAlarmText(out.Alarms[0])
				}
				return describeAlarm(out.Alarms[0])
			default:
				switch ringing {
				case 0:
					next := out.Alarms[0]
					return fmt.Sprintf("You have %d alarms. The next is at %s.",
						len(out.Alarms), formatClock(next.Hour, next.Minute))
				case 1:
					if label := out.Alarms[ringingIndex].Label; label != "" {
						return fmt.Sprintf("You have %d alarms. The %s alarm is ringing.",
							len(out.Alarms), label)
					}
					return fmt.Sprintf("You have %d alarms. One is ringing.", len(out.Alarms))
				default:
					return fmt.Sprintf("You have %d alarms. %d are ringing.", len(out.Alarms), ringing)
				}
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

// NewUpdate builds alarm.update. It changes the per-Alarm overrides of one
// existing Alarm, and can rename or retime it in place without creating a
// second Alarm. Anything not requested stays as it is.
func NewUpdate(m *Manager) tool.Tool[UpdateIn, UpdateOut] {
	return tool.Tool[UpdateIn, UpdateOut]{
		Name:        UpdateName,
		Description: "Change an existing alarm's sound, volume or ramp, rename it, or move it to a new time; the repeat rule stays unchanged.",
		Permission:  tool.PermissionAct,
		// The result embeds the stored Alarm, including a label the request may
		// not name when it matches by time or id, or when only one Alarm exists.
		// It stays on the Pi like alarm.list: the Agent, Hermes and MCP surfaces
		// cannot see or call it (#322).
		LocalOnly: tool.LocalOnlyYes,
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
			"rename my 7 am alarm to gym",
			"call my 7 am alarm work",
			"move my gym alarm to 6:30",
			"change my 7 am alarm to 6:30",
			"rename my alarm to gym",
			"move my work alarm to 8",
		},
		Extract: extractUpdate,
	}
}

// NewSnooze builds alarm.snooze. It re-arms the Alarm that is currently
// ringing for a spoken duration, or for the live ring.alarm.snooze default
// when the phrase carries no duration. The bare "snooze" control phrase still
// goes straight to the ringing alert through Focus and keeps its behaviour.
func NewSnooze(m *Manager) tool.Tool[SnoozeIn, SnoozeOut] {
	return tool.Tool[SnoozeIn, SnoozeOut]{
		Name:        SnoozeName,
		Description: "Snooze the alarm that is currently ringing, optionally for a spoken duration; with no duration it uses the configured snooze time.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in SnoozeIn) (SnoozeOut, error) {
			return m.Snooze(ctx, in)
		},
		Speak: speakSnooze,
		Examples: []string{
			"snooze the alarm for ten minutes",
			"snooze the alarm for 10 minutes",
			"snooze my alarm for twenty minutes",
			"snooze for five more minutes",
			"snooze the alarm",
			"snooze my alarm",
			"snooze the morning alarm for 15 minutes",
			"snooze the alarm for another ten minutes",
		},
		Extract: extractSnooze,
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
	if err := tool.Add(r, NewUpdate(m)); err != nil {
		return err
	}
	return tool.Add(r, NewSnooze(m))
}

// extractSnooze reads a phrased snooze request. The word "snooze" plus at
// least one other word is required: the bare control phrase is routed to the
// ringing alert through Focus and keeps its behaviour, while a bare duration
// such as "give me ten more minutes" is left to the timer or Agent paths. The
// delay uses the shared duration parser; with no spoken duration the Handler
// applies the live ring.alarm.snooze default.
func extractSnooze(text string) (SnoozeIn, bool) {
	words := wordsOf(text)
	snoozing := false
	for _, word := range words {
		if word == "snooze" || word == "snoozing" {
			snoozing = true
		}
	}
	if !snoozing || len(words) < 2 {
		return SnoozeIn{}, false
	}
	// Drop the filler words in "five more minutes" / "another ten minutes" so
	// the shared duration parser still reads the number and its unit.
	delayText := strings.ReplaceAll(text, " more ", " ")
	delayText = strings.ReplaceAll(delayText, " another ", " ")
	in := SnoozeIn{}
	if d, ok := parse.Duration(delayText, 0); ok && d > 0 {
		in.DurationSeconds = int(d.Round(time.Second) / time.Second)
	}
	return in, true
}

// speakSnooze renders the one-line confirmation, or answers clearly when no
// Alarm was ringing.
func speakSnooze(out SnoozeOut) string {
	if !out.Snoozed {
		return "There's no alarm ringing to snooze."
	}
	if out.DurationSeconds > 0 {
		return "Alarm snoozed for " + parse.HumanDuration(time.Duration(out.DurationSeconds)*time.Second) + "."
	}
	return SnoozeText
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

// editToValue reads the value of a trailing "to ..." / "as ..." on an edit
// request. callAlarmName reads the rename form without "to": "call my 7 am
// alarm work". Both are cut out before the override parser sees them, so a
// spoken rename is never mistaken for a sound change.
var (
	editToValue   = regexp.MustCompile(`(?i)\b(?:to|as)\s+(.+?)\s*[.!?]*$`)
	callAlarmName = regexp.MustCompile(`(?i)\bcall\s+(?:the\s+|my\s+|our\s+|your\s+)?(.+?)\s+alarm\s+(.+?)\s*[.!?]*$`)
)

// alarmEdit is the rename/retime half of a spoken update. stripped is the
// request with the new value removed, leaving only the selector.
type alarmEdit struct {
	newLabel *string
	newTime  string
	stripped string
}

// parseAlarmEdit reads a new name or clock from one utterance, reusing the
// shared clock parser. Volume and ramp clauses are ignored while scanning, so
// "update my alarm volume to 10" cannot become a retime to 10:00.
func parseAlarmEdit(text string) alarmEdit {
	edit := alarmEdit{stripped: text}
	scan := stripVolumeAndRamp(text)
	lower := strings.ToLower(scan)
	cut := func(clause string) {
		if clause != "" {
			edit.stripped = strings.TrimSpace(strings.Replace(text, clause, "", 1))
		}
	}

	if loc := editToValue.FindStringSubmatchIndex(scan); loc != nil {
		value := strings.TrimSpace(scan[loc[2]:loc[3]])
		_, _, isClock := parse.Clock(value)
		switch {
		case renameVerb(lower):
			if label := cleanAlarmLabel(value); label != "" {
				edit.newLabel = &label
				cut(scan[loc[0]:loc[3]])
			}
		case isClock && (retimeVerb(lower) || editVerb(lower)):
			edit.newTime = formatTimeValue(value)
			cut(scan[loc[0]:loc[3]])
		}
	}
	if edit.newLabel == nil && edit.newTime == "" {
		if loc := callAlarmName.FindStringSubmatchIndex(scan); loc != nil {
			if label := cleanAlarmLabel(scan[loc[4]:loc[5]]); label != "" {
				edit.newLabel = &label
				cut(scan[loc[4]:loc[5]])
			}
		}
	}
	return edit
}

// formatTimeValue renders a parsed clock as the HH:MM stored in UpdateIn.
func formatTimeValue(value string) string {
	hour, minute, ok := parse.Clock(value)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

// renameVerb, retimeVerb and editVerb classify the cue before a new value.
// A generic "change ... to X" only moves the time: a word like "birds" still
// belongs to the sound parser, so existing override requests keep working.
func renameVerb(lower string) bool {
	return containsWord(lower, "rename") || containsWord(lower, "call") ||
		containsWord(lower, "name") || containsWord(lower, "label")
}

func retimeVerb(lower string) bool {
	return containsWord(lower, "move") || containsWord(lower, "retime") ||
		containsWord(lower, "shift") || containsWord(lower, "reschedule")
}

func editVerb(lower string) bool {
	for _, verb := range []string{"change", "update", "switch", "edit", "modify", "adjust"} {
		if containsWord(lower, verb) {
			return true
		}
	}
	return false
}

// extractUpdate reads the selector, the rename/retime value and any per-Alarm
// override change. It requires an alarm word and an edit cue, so "wake me at 7
// with the birds sound" stays with alarm.set.
func extractUpdate(text string) (UpdateIn, bool) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "alarm") {
		return UpdateIn{}, false
	}
	if containsWord(lower, "wake") || !looksLikeAlarmUpdate(lower) {
		return UpdateIn{}, false
	}

	edit := parseAlarmEdit(text)
	overrides := parseOverrides(edit.stripped)
	in := UpdateIn{
		NewLabel:    edit.newLabel,
		NewTime:     edit.newTime,
		Sound:       overrides.Sound,
		SoundReset:  overrides.SoundReset,
		Volume:      overrides.Volume,
		VolumeReset: overrides.VolumeReset,
		RampSeconds: overrides.RampSeconds,
		RampReset:   overrides.RampReset,
	}
	if !in.hasChanges() {
		return UpdateIn{}, false
	}
	// The selector is read after the new value and override clauses are removed,
	// so "move my gym alarm to 6:30" selects by gym, not by 6:30.
	selector := stripOverridePhrases(edit.stripped)
	if hour, minute, ok := parse.Clock(selector); ok {
		in.Time = fmt.Sprintf("%02d:%02d", hour, minute)
	}
	in.Label = labelForAlarm(selector)
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

// speakUpdate says exactly what changed on the matched Alarm. A rename or
// retime leads with the named Alarm and its new value; unrequested overrides
// are never mentioned.
func speakUpdate(out UpdateOut) string {
	if out.Ambiguous {
		return "More than one alarm matches. Which one should I change?"
	}
	if out.Matched == 0 {
		return "No alarm matched."
	}
	labelChanged := changeIncludes(out.Changed, "label")
	timeChanged := changeIncludes(out.Changed, "time")
	oldClock := out.PreviousClock
	if oldClock == "" {
		oldClock = formatClock(out.Alarm.Hour, out.Alarm.Minute)
	}
	newClock := formatClock(out.Alarm.Hour, out.Alarm.Minute)

	target := "your " + newClock + " alarm"
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
	fallback := ""
	if out.SoundFallback {
		fallback = ", but I couldn't find that sound, so it uses the default sound"
	}
	if labelChanged || timeChanged {
		var lead string
		switch {
		case labelChanged && timeChanged:
			lead = "Renamed your " + oldClock + " alarm to " + out.Alarm.Label +
				" and moved it to " + newClock
		case labelChanged:
			lead = "Renamed your " + oldClock + " alarm to " + out.Alarm.Label
		default:
			name := "your " + oldClock + " alarm"
			if out.Alarm.Label != "" {
				name = capitalize(out.Alarm.Label) + " alarm"
			}
			lead = name + " moved to " + newClock
		}
		if len(parts) > 0 {
			lead += ", " + joinPhrases(parts)
		}
		return lead + fallback + "."
	}
	if out.SoundFallback {
		prefix := "Changed " + target
		if len(parts) > 0 {
			prefix += ": " + joinPhrases(parts)
		}
		return prefix + fallback + "."
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

// changeIncludes reports whether the reply names one kind of change.
func changeIncludes(changes []string, want string) bool {
	for _, change := range changes {
		if change == want {
			return true
		}
	}
	return false
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

// ringingAlarmText names the Alarm the user can hear, in the same style as the
// Timer and Reminder replies.
func ringingAlarmText(alarm Alarm) string {
	if alarm.Label != "" {
		return "Your " + alarm.Label + " alarm is ringing."
	}
	return "Your alarm is ringing."
}
