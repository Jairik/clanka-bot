package alarm

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

// The spoken per-Alarm overrides are parsed in one place so alarm.set,
// alarm.update and their Fast-path Extract functions agree. A sound name is
// only a candidate here; the Manager resolves it through the shared name
// matcher before anything is stored.
var (
	volumeAfter  = regexp.MustCompile(`(?i)\bvolume\s*(?:to|at|of|=|:)?\s*(\d{1,3})\s*(?:%|percent)?\b`)
	volumeBefore = regexp.MustCompile(`(?i)\b(\d{1,3})\s*(?:%|percent)\s*(?:volume|gain)\b`)
	volumeZero   = regexp.MustCompile(`(?i)\bvolume\s+(?:to\s+|at\s+)?zero\b|\bzero\s+volume\b`)
	volumeReset  = regexp.MustCompile(`(?i)\b(?:default|reset|clear|remove)\s+(?:the\s+)?volume\b|\bvolume\s+(?:reset|default|off)\b|\b(?:default|reset|clear|remove)\s+(?:the\s+|my\s+|our\s+)?alarm(?:'s)?\s+volume\b`)

	rampAfter  = regexp.MustCompile(`(?i)\bramp\s*(?:to|at|of|for|over|=|:)?\s*(\d{1,4})\s*(seconds?|secs?|s|minutes?|mins?)\b`)
	rampBefore = regexp.MustCompile(`(?i)\b(\d{1,4})\s*(seconds?|secs?|s|minutes?|mins?)\s+ramp\b`)
	rampFade   = regexp.MustCompile(`(?i)\b(?:fade\s+in|ramp\s+up)\s+(?:over\s+|for\s+)?(\d{1,4})\s*(seconds?|secs?|s|minutes?|mins?)\b`)
	rampZero   = regexp.MustCompile(`(?i)\bno\s+ramp\b|\bwithout\s+(?:a\s+)?ramp\b|\bramp\s+(?:off|none|zero)\b`)
	rampReset  = regexp.MustCompile(`(?i)\b(?:default|reset|clear|remove)\s+(?:the\s+)?ramp\b|\bramp\s+(?:reset|default)\b|\b(?:default|reset|clear|remove)\s+(?:the\s+|my\s+|our\s+)?alarm(?:'s)?\s+ramp\b`)

	soundReset = regexp.MustCompile(`(?i)\b(?:default|reset|clear|remove)\s+(?:the\s+)?sound\b|\b(?:no|without)\s+(?:a\s+)?sound\b|\bsound\s+(?:reset|default|off)\b|\b(?:default|reset|clear|remove)\s+(?:the\s+|my\s+|our\s+)?alarm(?:'s)?\s+sound\b`)
	soundOf    = regexp.MustCompile(`(?i)\bsound\s+of\s+(?:my|the|our)\s+alarm\s+(?:to|should be)\s+(.+)$`)
	soundWith  = regexp.MustCompile(`(?i)\b(?:with|using|use)\s+(.+?)\s+sounds?\b`)
	soundTo    = regexp.MustCompile(`(?i)\b(?:to|as)\s+(.+?)\s+sounds?\b`)
	soundFor   = regexp.MustCompile(`(?i)\b(?:sound|tone)\s+(?:to|as)\s+(.+)$`)
	soundBare  = regexp.MustCompile(`(?i)\b(?:with|using|use)\s+(.+)$`)
	soundToEnd = regexp.MustCompile(`(?i)\bto\s+(.+)$`)

	// alarmOverridePossessive catches "my alarm volume" and "the sound of my
	// alarm", which mark an update even when the verb is "set".
	alarmOverridePossessive = regexp.MustCompile(`(?i)\balarm(?:'s)?\s+(?:sound|volume|ramp)\b|\b(?:sound|volume|ramp)\s+of\s+(?:my|the|our)\s+alarm\b`)
)

// overrideIntent is one parsed set of spoken overrides. A nil pointer or empty
// Sound means "no change", while the Reset flags clear the stored value. The
// explicit zero values are preserved: Volume=0 and RampSeconds=0 are real
// overrides, not the default.
type overrideIntent struct {
	Sound       string
	SoundReset  bool
	Volume      *int
	VolumeReset bool
	RampSeconds *int
	RampReset   bool
}

// hasChanges reports whether anything should be written.
func (o overrideIntent) hasChanges() bool {
	return o.Sound != "" || o.SoundReset ||
		o.Volume != nil || o.VolumeReset ||
		o.RampSeconds != nil || o.RampReset
}

// parseOverrides reads the optional sound, volume and ramp from one utterance.
func parseOverrides(text string) overrideIntent {
	var out overrideIntent
	out.Sound, out.SoundReset = parseSound(text)
	out.Volume, out.VolumeReset = parseVolume(text)
	out.RampSeconds, out.RampReset = parseRamp(text)
	return out
}

// parseSound extracts a spoken library name. A reset phrase clears the
// override; an unmatched name is left for the Manager, which stores the kind
// default and reports the fallback instead of guessing a library entry.
func parseSound(text string) (string, bool) {
	if soundReset.MatchString(text) {
		return "", true
	}
	stripped := stripVolumeAndRamp(text)
	for _, pattern := range []*regexp.Regexp{soundOf, soundWith, soundTo, soundFor, soundBare, soundToEnd} {
		match := pattern.FindStringSubmatch(stripped)
		if match == nil {
			continue
		}
		if name := cleanSound(match[1]); name != "" {
			return name, false
		}
	}
	return "", false
}

// cleanSound keeps a plausible library name and drops the clock, schedule and
// override words the surrounding patterns can capture.
func cleanSound(raw string) string {
	text := strings.ToLower(strings.TrimSpace(raw))
	// Cut a trailing clause that belongs to another field or a sentence.
	for _, marker := range []string{" at ", " with ", " and ", ",", " please"} {
		if index := strings.Index(text, marker); index >= 0 {
			text = text[:index]
		}
	}
	text = strings.Trim(text, " .,;:!?'\"")
	for _, prefix := range []string{"the ", "a ", "an ", "my ", "our ", "your "} {
		text = strings.TrimPrefix(text, prefix)
	}
	text = strings.TrimSuffix(text, " sounds")
	text = strings.TrimSuffix(text, " sound")
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 80 {
		return ""
	}
	if _, _, ok := parse.Clock(text); ok {
		return ""
	}
	for _, word := range wordsOf(text) {
		switch word {
		case "volume", "ramp", "alarm", "default", "none", "sound", "off", "on",
			"it", "that", "this", "please":
			return ""
		}
	}
	if schedule, _ := parseScheduleText(text); schedule != Once {
		return ""
	}
	if !strings.ContainsAny(text, "abcdefghijklmnopqrstuvwxyz") {
		return ""
	}
	return text
}

// parseVolume reads the explicit volume or the reset flag. A percentage before
// the word ("40 percent volume") and after it ("volume to 40") both work.
func parseVolume(text string) (*int, bool) {
	if volumeReset.MatchString(text) {
		return nil, true
	}
	for _, pattern := range []*regexp.Regexp{volumeAfter, volumeBefore} {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		value, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		return &value, false
	}
	if volumeZero.MatchString(text) {
		zero := 0
		return &zero, false
	}
	return nil, false
}

// parseRamp reads the explicit ramp seconds or the reset flag. "no ramp" is an
// explicit zero (no ramp); "default ramp" clears back to ring.alarm.ramp.
func parseRamp(text string) (*int, bool) {
	if rampReset.MatchString(text) {
		return nil, true
	}
	if rampZero.MatchString(text) {
		zero := 0
		return &zero, false
	}
	for _, pattern := range []*regexp.Regexp{rampAfter, rampBefore, rampFade} {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		value, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(match[2]), "m") {
			value *= 60
		}
		return &value, false
	}
	return nil, false
}

// stripVolumeAndRamp removes the volume and ramp clauses before the sound is
// parsed, so "with the birds sound at volume 40" still yields "birds".
func stripVolumeAndRamp(text string) string {
	out := volumeAfter.ReplaceAllString(text, " ")
	out = volumeBefore.ReplaceAllString(out, " ")
	out = volumeZero.ReplaceAllString(out, " ")
	out = rampAfter.ReplaceAllString(out, " ")
	out = rampBefore.ReplaceAllString(out, " ")
	out = rampFade.ReplaceAllString(out, " ")
	out = rampZero.ReplaceAllString(out, " ")
	return out
}

// stripOverridePhrases removes the override clauses before a selector clock is
// parsed, so "change my alarm volume to 10" does not treat 10 as an alarm time.
func stripOverridePhrases(text string) string {
	out := stripVolumeAndRamp(text)
	out = soundOf.ReplaceAllString(out, " ")
	out = soundWith.ReplaceAllString(out, " ")
	out = soundTo.ReplaceAllString(out, " ")
	out = soundFor.ReplaceAllString(out, " ")
	return out
}

// looksLikeAlarmUpdate reports whether the utterance asks to change an
// existing Alarm rather than create a new one. It is the guard that stops
// "set an alarm for 7 with the birds sound" from being read as an update while
// still accepting "set my alarm volume to 40".
func looksLikeAlarmUpdate(lower string) bool {
	if soundOf.MatchString(lower) || alarmOverridePossessive.MatchString(lower) {
		return true
	}
	for _, verb := range []string{"change", "update", "switch", "modify", "adjust", "edit", "reset"} {
		if containsWord(lower, verb) {
			return true
		}
	}
	hasOverride := containsWord(lower, "sound") || containsWord(lower, "volume") || containsWord(lower, "ramp")
	if hasOverride {
		for _, verb := range []string{"remove", "clear", "use"} {
			if containsWord(lower, verb) {
				return true
			}
		}
	}
	// "make my alarm use birds" is an update, but "make me wake up at 7" is not.
	if containsWord(lower, "make") && containsWord(lower, "alarm") && containsWord(lower, "use") {
		return true
	}
	return false
}

// containsWord reports whether word appears as a whole word.
func containsWord(text, word string) bool {
	for _, candidate := range wordsOf(text) {
		if candidate == word {
			return true
		}
	}
	return false
}
