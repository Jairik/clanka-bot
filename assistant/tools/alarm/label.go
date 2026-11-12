package alarm

import (
	"regexp"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

// labelPatterns pull the optional spoken name out of the natural forms:
//
//	set a work alarm for 7:30
//	set an alarm called work for 7:30
//	cancel my work alarm
//
// The clock time and repeat rule are parsed elsewhere; these only isolate words
// that are not part of them.
var (
	labelBeforeAlarm = regexp.MustCompile(`\b(?:set|create|make|add)(?:\s+(?:me|us))?(?:\s+(?:a|an|the|another))?\s+([a-z0-9][a-z0-9' -]*?)\s+alarm\b`)
	labelCalled      = regexp.MustCompile(`\b(?:called|named|labelled|labeled)\s+([a-z0-9][a-z0-9' -]*)`)
	labelCancelAlarm = regexp.MustCompile(`\b(?:cancel|delete|remove|clear|turn off|get rid of)\s+(?:the\s+|my\s+|our\s+)?([a-z0-9][a-z0-9' -]*?)\s+alarm\b`)
	labelTrim        = regexp.MustCompile(`^[^a-z0-9]+|[^a-z0-9]+$`)
)

// alarmLabelStopwords are articles and pronouns that are not a name.
var alarmLabelStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "my": true, "our": true, "your": true,
	"another": true, "new": true, "this": true, "that": true, "it": true,
	"wakeup": true, "wake up": true,
}

// labelForAlarm returns the spoken label in text, or "" when there is none.
// "wake me at 7" and "cancel my 7 am alarm" intentionally have no label.
func labelForAlarm(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return ""
	}
	if match := labelCalled.FindStringSubmatch(lower); match != nil {
		if label := cleanAlarmLabel(cutAtSchedule(match[1])); label != "" {
			return label
		}
	}
	if match := labelBeforeAlarm.FindStringSubmatch(lower); match != nil {
		if label := cleanAlarmLabel(match[1]); label != "" {
			return label
		}
	}
	if match := labelCancelAlarm.FindStringSubmatch(lower); match != nil {
		if label := cleanAlarmLabel(match[1]); label != "" {
			return label
		}
	}
	return ""
}

// cleanAlarmLabel normalises a captured name and rejects the non-labels the
// patterns can collect, including a captured clock time like "7 am".
func cleanAlarmLabel(raw string) string {
	label := strings.ToLower(strings.TrimSpace(raw))
	label = labelTrim.ReplaceAllString(label, "")
	label = strings.Join(strings.Fields(label), " ")
	for _, article := range []string{"the ", "my ", "our ", "your ", "another ", "a ", "an "} {
		label = strings.TrimPrefix(label, article)
	}
	label = strings.TrimSuffix(label, " alarm")
	if label == "" || alarmLabelStopwords[label] || looksLikeClockLabel(label) {
		return ""
	}
	if !strings.ContainsAny(label, "abcdefghijklmnopqrstuvwxyz") {
		return ""
	}
	// A spoken label is a name, not a sentence; keep it short.
	if len(label) > 40 {
		label = label[:40]
	}
	return strings.TrimSpace(label)
}

// cutAtSchedule drops a trailing time or repeat phrase from a captured label,
// so "gym for 6:30" becomes "gym" and "work on weekdays" becomes "work".
func cutAtSchedule(text string) string {
	for _, marker := range []string{" for ", " at ", " on ", " every "} {
		index := strings.Index(text, marker)
		if index < 0 {
			continue
		}
		rest := text[index+len(marker):]
		if _, _, ok := parse.Clock(rest); ok {
			return text[:index]
		}
		if schedule, _ := parseScheduleText(rest); schedule != Once {
			return text[:index]
		}
	}
	return text
}

// looksLikeClockLabel reports whether every word is a clock word, so a capture
// like "7 am" is rejected as a label.
func looksLikeClockLabel(label string) bool {
	fields := strings.Fields(label)
	if len(fields) == 0 {
		return false
	}
	for _, field := range fields {
		word := strings.Trim(field, `.,;:!?`)
		if isDigits(word) {
			continue
		}
		switch word {
		case "am", "pm", "a", "p", "m", "oclock", "o'clock", "at", "for", "on", "every":
			continue
		default:
			return false
		}
	}
	return true
}

// isDigits reports whether s is a non-empty digit run.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
