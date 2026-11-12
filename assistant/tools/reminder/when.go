package reminder

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
)

// The parser reads the two time forms a Reminder accepts. Duration phrases use
// the shared parser; clock phrases are recognised here because the shared
// parser deliberately reads durations only. The message is whatever remains
// after the time phrase and the request prefix are removed.

// span is one removed region of the original utterance.
type span struct {
	start int
	end   int
}

var (
	// clockAt is the explicit clock form: "at 5", "at 5:30 pm", "around 7am".
	clockAt = regexp.MustCompile(`(?i)\b(?:at|by|around)\s+(\d{1,2})(?::([0-5]\d))?\s*(a\.?m\.?|p\.?m\.?)?(?:\s*o'?clock)?`)
	// clockMeridiem is the same without a preposition: "remind me 5 pm to ...".
	clockMeridiem = regexp.MustCompile(`(?i)\b(\d{1,2})(?::([0-5]\d))?\s*(a\.?m\.?|p\.?m\.?)\b`)
	// clockColon is a 24-hour time with no meridiem: "6:30", "17:45".
	clockColon = regexp.MustCompile(`\b(\d{1,2}):([0-5]\d)\b`)
	// clockWord covers the two spoken fixed points.
	clockWord = regexp.MustCompile(`(?i)\b(noon|midnight)\b`)
	// dayTomorrow and dayToday qualify the date rather than the message.
	dayTomorrow = regexp.MustCompile(`(?i)\btomorrow\b`)
	dayToday    = regexp.MustCompile(`(?i)\btoday\b`)
	// durationUnitAfter rejects "at 5 minutes" as a clock time.
	durationUnitAfter = regexp.MustCompile(`(?i)^\s*(?:seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h)\b`)

	// requestPrefix is the reminder request itself, removed from the message.
	requestPrefix = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+)?(?:remind\s+(?:me|us)|set\s+(?:me\s+)?(?:a|an|the)?\s*reminder|set\s+reminder|make\s+(?:a|an)?\s*reminder|create\s+(?:a|an)?\s*reminder|reminder)\b`)
	// bareReminder identifies a statement ("reminder that ...") rather than a request.
	bareReminder = regexp.MustCompile(`(?i)^\s*(?:please\s+)?reminder\b`)
	// leadingConnector and trailingConnector remove sentence glue left behind.
	leadingConnector  = regexp.MustCompile(`(?i)^\s*(?:to|that|about|and|for|in|at|by|on)\b[\s,]*`)
	trailingConnector = regexp.MustCompile(`(?i)[\s,]+(?:to|that|about|and|for|in|at|by|on)\s*$`)
	// messageEdge trims punctuation that belongs to the sentence, not the message.
	messageEdge = regexp.MustCompile(`^[\s,.;:!?-]+|[\s,.;:!?-]+$`)

	// cancelPrefix is the cancel request itself. The optional reminder word is
	// consumed when it is present, leaving a message query or an empty query.
	cancelPrefix = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+)?(?:cancel|delete|remove|forget|clear|drop|get\s+rid\s+of|stop)\s+(?:the\s+|my\s+|our\s+|all\s+|any\s+)?(?:reminders?|alerts?)?\b`)
	// cancelTrailer removes the noun from "cancel the oven reminder".
	cancelTrailer = regexp.MustCompile(`(?i)[\s,]+(?:reminders?|alerts?)\s*$`)
	// wordSpan finds the words a duration phrase can occupy.
	wordSpan = regexp.MustCompile(`[A-Za-z0-9]+`)
)

// nextClockTime resolves one local clock time to an instant in loc. A time
// already reached (or exactly now) is the next day, matching the Alarm policy:
// "remind me at 5" after 5 pm means tomorrow at 5. An explicit "tomorrow" is
// kept as tomorrow rather than rolled forward twice.
func nextClockTime(now time.Time, hour, minute, dayOffset int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	candidate := time.Date(local.Year(), local.Month(), local.Day()+dayOffset, hour, minute, 0, 0, loc)
	if !candidate.After(now) {
		candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, hour, minute, 0, 0, loc)
	}
	return candidate
}

// parseSetInput reads one set request into its Tool input. It accepts either a
// duration or a clock time and requires a message; a bare "remind me in ten
// minutes" has nothing to say when it is due and is left to the Agent path.
func parseSetInput(text string) (SetIn, bool) {
	original := strings.TrimSpace(text)
	if original == "" {
		return SetIn{}, false
	}
	match := requestPrefix.FindStringIndex(original)
	if match == nil {
		return SetIn{}, false
	}
	rest := original[match[1]:]
	// "reminder that the meeting is at noon" is a statement, not a request.
	if bareReminder.MatchString(original) && strings.HasPrefix(strings.ToLower(strings.TrimSpace(rest)), "that ") {
		return SetIn{}, false
	}

	removed := make([]span, 0, 3)
	for _, re := range []*regexp.Regexp{dayTomorrow, dayToday} {
		if loc := re.FindStringIndex(original); loc != nil {
			removed = append(removed, span{loc[0], loc[1]})
		}
	}

	message := rest
	duration, hasDuration := parse.Duration(original, time.Minute)
	durationStart, durationEnd, hasDurationSpan := 0, 0, false
	if hasDuration && duration > 0 {
		durationStart, durationEnd, hasDurationSpan = durationSpan(original, duration)
	}
	if hour, minute, tomorrow, loc, ok := parseClockSpan(original); ok {
		// The first time phrase wins: "in 20 minutes to meet at 5" schedules
		// in 20 minutes, while "at 5 pm to call in 20 minutes" is a clock
		// reminder whose message happens to contain a duration.
		if !hasDurationSpan || loc[0] <= durationStart {
			removed = append(removed, span{loc[0], loc[1]})
			message = stripSpans(rest, spansRelative(removed, match[1]))
			message = cleanMessage(message)
			if message == "" {
				return SetIn{}, false
			}
			return SetIn{
				Message:  message,
				At:       fmt.Sprintf("%02d:%02d", hour, minute),
				Tomorrow: tomorrow,
			}, true
		}
	}

	if !hasDuration || duration <= 0 {
		return SetIn{}, false
	}
	if hasDurationSpan {
		removed = append(removed, span{durationStart, durationEnd})
	}
	message = stripSpans(rest, spansRelative(removed, match[1]))
	message = cleanMessage(message)
	if message == "" {
		return SetIn{}, false
	}
	return SetIn{
		Message:         message,
		DurationSeconds: int(duration.Round(time.Second) / time.Second),
	}, true
}

// spansRelative converts absolute spans into spans of the text after the
// request prefix, dropping the ones that fall before it.
func spansRelative(spans []span, offset int) []span {
	out := make([]span, 0, len(spans))
	for _, item := range spans {
		if item.end <= offset {
			continue
		}
		if item.start < offset {
			item.start = offset
		}
		out = append(out, span{item.start - offset, item.end - offset})
	}
	return out
}

// parseClockSpan finds a clock time in the full utterance. The match location
// is absolute so the caller can remove it from the message.
func parseClockSpan(full string) (hour, minute int, tomorrow bool, loc []int, ok bool) {
	tomorrow = dayTomorrow.MatchString(full)

	if m := clockAt.FindStringSubmatchIndex(full); m != nil {
		if hour, minute, ok := clockValues(full, m); ok {
			return hour, minute, tomorrow, m[:2], true
		}
	}
	if m := clockMeridiem.FindStringSubmatchIndex(full); m != nil {
		if hour, minute, ok := clockValues(full, m); ok {
			return hour, minute, tomorrow, m[:2], true
		}
	}
	if m := clockColon.FindStringSubmatchIndex(full); m != nil {
		hour := atoi(full[m[2]:m[3]])
		minute := atoi(full[m[4]:m[5]])
		if hour <= 23 && minute <= 59 {
			return hour, minute, tomorrow, m[:2], true
		}
	}
	if m := clockWord.FindStringSubmatchIndex(full); m != nil {
		switch strings.ToLower(full[m[2]:m[3]]) {
		case "noon":
			return 12, 0, tomorrow, m[:2], true
		case "midnight":
			return 0, 0, tomorrow, m[:2], true
		}
	}
	return 0, 0, false, nil, false
}

// clockValues reads one clock regex match. A bare "at 5 minutes" is a
// duration, not a clock time, and is refused.
func clockValues(text string, m []int) (int, int, bool) {
	hourText := text[m[2]:m[3]]
	hour, err := strconv.Atoi(hourText)
	if err != nil {
		return 0, 0, false
	}
	minute := 0
	if m[4] >= 0 {
		minute, _ = strconv.Atoi(text[m[4]:m[5]])
	}
	meridiem := ""
	if m[6] >= 0 {
		meridiem = strings.ToLower(strings.NewReplacer(".", "", " ", "").Replace(text[m[6]:m[7]]))
	}
	// A duration unit immediately after the number means this is not a time.
	if durationUnitAfter.MatchString(text[m[1]:]) {
		return 0, 0, false
	}
	if meridiem != "" {
		if hour < 1 || hour > 12 {
			return 0, 0, false
		}
		switch meridiem {
		case "am":
			if hour == 12 {
				hour = 0
			}
		case "pm":
			if hour != 12 {
				hour += 12
			}
		default:
			return 0, 0, false
		}
	} else if hour > 23 {
		return 0, 0, false
	}
	return hour, minute, true
}

// durationSpan finds the shortest word span that parses to exactly d, starting
// on a number-like word and ending on a unit (or a fraction), so the time
// phrase is removed whole and a message verb is never swallowed.
func durationSpan(text string, d time.Duration) (int, int, bool) {
	locs := wordSpan.FindAllStringIndex(text, -1)
	for size := 1; size <= 7; size++ {
		for i := 0; i+size <= len(locs); i++ {
			first := text[locs[i][0]:locs[i][1]]
			last := text[locs[i+size-1][0]:locs[i+size-1][1]]
			if !durationStart(first) || !durationEnd(last) {
				continue
			}
			start, end := locs[i][0], locs[i+size-1][1]
			if got, ok := parse.Duration(text[start:end], 0); ok && got == d {
				return start, end, true
			}
		}
	}
	return 0, 0, false
}

// durationStart reports whether one token can begin a duration phrase.
func durationStart(word string) bool {
	lower := strings.ToLower(word)
	if lower == "a" || lower == "an" || lower == "half" || lower == "quarter" {
		return true
	}
	if _, err := strconv.Atoi(lower); err == nil {
		return true
	}
	switch lower {
	case "one", "two", "three", "four", "five", "six", "seven", "eight",
		"nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
		"sixteen", "seventeen", "eighteen", "nineteen", "twenty", "thirty",
		"forty", "fifty", "sixty", "seventy", "eighty", "ninety", "hundred":
		return true
	default:
		return false
	}
}

// durationEnd reports whether one token can finish a duration phrase.
func durationEnd(word string) bool {
	switch strings.ToLower(word) {
	case "half", "quarter":
		return true
	default:
		return durationWord(word)
	}
}

// durationWord reports whether one token names a duration unit.
func durationWord(word string) bool {
	switch strings.ToLower(word) {
	case "s", "sec", "secs", "second", "seconds",
		"m", "min", "mins", "minute", "minutes",
		"h", "hr", "hrs", "hour", "hours":
		return true
	default:
		return false
	}
}

// stripSpans removes the given regions and inserts a space so words cannot
// glue together. Spans are sorted and overlapping spans are ignored.
func stripSpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	last := 0
	for _, item := range spans {
		if item.start < last || item.start < 0 || item.end > len(text) {
			continue
		}
		b.WriteString(text[last:item.start])
		b.WriteByte(' ')
		last = item.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// cleanMessage strips the sentence glue and punctuation left around the
// message. It repeats until stable so "for to stretch" loses both words.
func cleanMessage(message string) string {
	current := strings.TrimSpace(message)
	for {
		next := leadingConnector.ReplaceAllString(current, "")
		next = trailingConnector.ReplaceAllString(next, "")
		next = messageEdge.ReplaceAllString(next, "")
		next = strings.Join(strings.Fields(next), " ")
		if next == current {
			return next
		}
		current = next
	}
}

// cancellationQuery reads which reminder a cancel request names. An empty
// query means every reminder. A Timer or Alarm request is refused so the
// shared "cancel" verbs cannot target a Reminder by accident.
func cancellationQuery(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return ""
	}
	if !strings.Contains(lower, "reminder") && (strings.Contains(lower, "timer") || strings.Contains(lower, "alarm")) {
		return ""
	}
	match := cancelPrefix.FindStringIndex(text)
	if match == nil {
		// A bare message phrase is still useful ("cancel the oven reminder" with
		// an unusual verb order); the semantic router only sends reminder-like
		// utterances here.
		return cleanMessage(cancelTrailer.ReplaceAllString(text, ""))
	}
	rest := text[match[1]:]
	rest = cancelTrailer.ReplaceAllString(rest, "")
	return cleanMessage(rest)
}

// atoi reads a small decimal without surfacing an error to the caller.
func atoi(text string) int {
	n, _ := strconv.Atoi(text)
	return n
}
