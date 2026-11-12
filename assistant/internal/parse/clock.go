package parse

import (
	"regexp"
	"strconv"
	"strings"
)

// clockWords are the named times a spoken request may use instead of digits.
var clockWords = map[string]struct{ hour, minute int }{
	"noon":     {12, 0},
	"midnight": {0, 0},
}

// clockPattern matches a clock time: "7", "7am", "7 am", "6:30", "6:30 pm",
// "7 o'clock", "noon" or "midnight". The optional am/pm suffix decides a
// 12-hour reading; without one the number is a 24-hour hour.
var clockPattern = regexp.MustCompile(`(?i)\b(noon|midnight)\b|\b([0-9]{1,2})(?::([0-9]{2}))?\s*(a\.?\s*m\.?|p\.?\s*m\.?)?(?:\s*o'?clock)?\b`)

// clockRejectUnits follow a bare number in a duration phrase. "10 minutes" is
// a length, not ten o'clock, so Clock skips that match.
var clockRejectUnits = map[string]bool{
	"s": true, "sec": true, "secs": true, "second": true, "seconds": true,
	"m": true, "min": true, "mins": true, "minute": true, "minutes": true,
	"h": true, "hr": true, "hrs": true, "hour": true, "hours": true,
	"day": true, "days": true, "week": true, "weeks": true,
}

// Clock returns the first clock time in text. It accepts 24-hour and 12-hour
// forms and treats noon and midnight by name. A bare number followed by a
// duration unit is not a clock time, so "set a timer for 10 minutes" yields no
// clock. The second result is false when no valid clock time is present.
func Clock(text string) (hour, minute int, ok bool) {
	normalized := normalizeClockText(text)
	for _, match := range clockPattern.FindAllStringSubmatchIndex(normalized, -1) {
		// A named time is complete on its own.
		if match[2] >= 0 {
			word := normalized[match[2]:match[3]]
			if named, exists := clockWords[word]; exists {
				return named.hour, named.minute, true
			}
			continue
		}

		hourText := normalized[match[4]:match[5]]
		hourValue, err := strconv.Atoi(hourText)
		if err != nil {
			continue
		}
		minuteValue := 0
		if match[6] >= 0 {
			minuteValue, err = strconv.Atoi(normalized[match[6]:match[7]])
			if err != nil || minuteValue > 59 {
				continue
			}
		}
		suffix := ""
		if match[8] >= 0 {
			suffix = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(normalized[match[8]:match[9]], " ", ""), ".", ""))
		}
		// A bare number immediately followed by a duration unit is a length.
		if suffix == "" {
			if next := firstWord(normalized[match[1]:]); clockRejectUnits[next] {
				continue
			}
		}

		value, valid := clockHour(hourValue, minuteValue, suffix)
		if valid {
			return value / 60, value % 60, true
		}
	}
	return 0, 0, false
}

// clockHour converts one parsed hour, minute and suffix into minutes since
// midnight. It rejects out-of-range values and a 12-hour suffix on a 24-hour
// hour.
func clockHour(hour, minute int, suffix string) (int, bool) {
	switch suffix {
	case "am", "a":
		if hour < 1 || hour > 12 {
			return 0, false
		}
		if hour == 12 {
			hour = 0
		}
	case "pm", "p":
		if hour < 1 || hour > 12 {
			return 0, false
		}
		if hour != 12 {
			hour += 12
		}
	default:
		if hour < 0 || hour > 23 {
			return 0, false
		}
	}
	return hour*60 + minute, true
}

// normalizeClockText lowercases text, expands the dotted am/pm spellings and
// replaces punctuation that can split a clock time with spaces.
func normalizeClockText(text string) string {
	normalized := strings.ToLower(strings.TrimSpace(text))
	replacer := strings.NewReplacer(
		"a.m.", "am", "p.m.", "pm",
		"a. m.", "am", "p. m.", "pm",
		"a.m", "am", "p.m", "pm",
	)
	normalized = replacer.Replace(normalized)
	normalized = strings.ReplaceAll(normalized, "’", "'")
	return normalized
}

// firstWord returns the first whitespace-separated word, without punctuation.
func firstWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], `.,;:!?'"`)
}
