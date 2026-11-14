package timer

import (
	"regexp"
	"strings"
)

// labelPatterns pull the spoken name out of the natural forms:
//
//	set a pasta timer for 10 minutes
//	set a timer for 10 minutes called pasta
//	how long is left on the pasta timer
//
// The duration itself is parsed by internal/parse; these only isolate words
// that are not part of it.
var (
	labelBeforeTimer = regexp.MustCompile(`\b(?:set|start|make|create)(?:\s+(?:me|us))?(?:\s+(?:a|an|the|another))?\s+([a-z0-9][a-z0-9' -]*?)\s+timer\b`)
	labelCalled      = regexp.MustCompile(`\b(?:called|named|labelled|labeled)\s+([a-z0-9][a-z0-9' -]*)`)
	labelCancelTimer = regexp.MustCompile(`\b(?:cancel|stop|end|kill|clear|get rid of)\s+(?:the\s+|my\s+|our\s+)?([a-z0-9][a-z0-9' -]*?)\s+timer\b`)
	labelOnTimer     = regexp.MustCompile(`\b(?:on|for|with)\s+(?:the\s+|my\s+|our\s+)?([a-z0-9][a-z0-9' -]*?)\s+timer\b`)
	timerWord        = regexp.MustCompile(`\btimer\b`)
	labelTrim        = regexp.MustCompile(`^[^a-z0-9]+|[^a-z0-9]+$`)
)

// labelStopwords are articles a pattern can capture when the sentence has no
// real label ("set a timer", "how long is left on my timer").
var labelStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "my": true, "our": true,
	"another": true, "new": true, "this": true, "that": true,
}

// durationWords are the words a duration phrase uses. A "label" made only of
// these is the timer length, not a name ("start a five minute timer").
var durationWords = map[string]bool{
	"a": true, "an": true, "and": true, "half": true, "quarter": true,
	"one": true, "two": true, "three": true, "four": true, "five": true,
	"six": true, "seven": true, "eight": true, "nine": true, "ten": true,
	"eleven": true, "twelve": true, "thirteen": true, "fourteen": true,
	"fifteen": true, "sixteen": true, "seventeen": true, "eighteen": true,
	"nineteen": true, "twenty": true, "thirty": true, "forty": true,
	"fifty": true, "sixty": true, "seventy": true, "eighty": true,
	"ninety": true, "hundred": true,
	"s": true, "sec": true, "secs": true, "second": true, "seconds": true,
	"m": true, "min": true, "mins": true, "minute": true, "minutes": true,
	"h": true, "hr": true, "hrs": true, "hour": true, "hours": true,
}

// labelForTimer returns the spoken label in text, or "" when there is none.
func labelForTimer(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return ""
	}
	if match := labelCalled.FindStringSubmatch(lower); match != nil {
		if label := cleanLabel(cutAtDuration(match[1])); label != "" {
			return label
		}
	}
	if match := labelBeforeTimer.FindStringSubmatch(lower); match != nil {
		if label := cleanLabel(match[1]); label != "" {
			return label
		}
	}
	if match := labelCancelTimer.FindStringSubmatch(lower); match != nil {
		if label := cleanLabel(match[1]); label != "" {
			return label
		}
	}
	if match := labelOnTimer.FindStringSubmatch(lower); match != nil {
		if label := cleanLabel(cutAtDuration(match[1])); label != "" {
			return label
		}
	}
	return closestLabel(lower)
}

// closestLabel reads the name immediately before the last "timer" in a
// sentence with no cue word ("when is the egg timer done"). It refuses to guess
// from a command sentence whose specific patterns already found nothing ("set a
// timer") or from a phrase whose nearest word is a function word ("how long is
// left on my timer").
func closestLabel(lower string) string {
	for _, command := range []string{
		"set ", "start ", "make ", "create ", "cancel ", "stop ", "end ",
		"kill ", "clear ", "get rid of ",
	} {
		if strings.Contains(lower, command) {
			return ""
		}
	}
	matches := timerWord.FindAllStringIndex(lower, -1)
	if len(matches) == 0 {
		return ""
	}
	fields := strings.Fields(lower[:matches[len(matches)-1][0]])
	index := len(fields) - 1
	for index >= 0 && labelArticles[trimWord(fields[index])] {
		index--
	}
	if index < 0 || labelFunctionWords[trimWord(fields[index])] || durationWords[trimWord(fields[index])] {
		return ""
	}
	var parts []string
	for ; index >= 0 && len(parts) < 2; index-- {
		word := trimWord(fields[index])
		if labelArticles[word] || labelFunctionWords[word] {
			break
		}
		parts = append([]string{word}, parts...)
	}
	return cleanLabel(strings.Join(parts, " "))
}

// labelArticles are the small words a label can follow.
var labelArticles = map[string]bool{
	"a": true, "an": true, "the": true, "my": true, "our": true, "your": true,
	"this": true, "that": true, "another": true, "new": true,
}

// labelFunctionWords are sentence glue, never part of a timer name.
var labelFunctionWords = map[string]bool{
	"on": true, "for": true, "with": true, "in": true, "at": true, "of": true,
	"to": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "when": true, "what": true, "whats": true, "which": true,
	"how": true, "left": true, "time": true, "until": true, "does": true,
	"do": true, "done": true, "goes": true, "go": true, "off": true,
	"still": true, "running": true, "active": true, "have": true, "has": true,
	"get": true, "rid": true, "and": true, "or": true, "not": true,
}

// trimWord strips the punctuation a transcript may leave on a word.
func trimWord(word string) string {
	return strings.Trim(word, `"'.,!?;:`)
}

// cutAtDuration drops a trailing "for 10 minutes" / "in five minutes" from a
// captured label, so "pasta for ten minutes" becomes "pasta".
func cutAtDuration(text string) string {
	for _, marker := range []string{" for ", " in ", " at "} {
		if index := strings.Index(text, marker); index >= 0 {
			rest := text[index+len(marker):]
			if looksLikeDuration(rest) {
				return text[:index]
			}
		}
	}
	return text
}

// looksLikeDuration reports whether every word is a number or a unit.
func looksLikeDuration(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	for _, field := range fields {
		if !durationWords[field] && !isDigits(field) {
			return false
		}
	}
	return true
}

// cleanLabel normalises a captured name and rejects the non-labels the
// patterns can collect.
func cleanLabel(raw string) string {
	label := strings.ToLower(strings.TrimSpace(raw))
	label = labelTrim.ReplaceAllString(label, "")
	label = strings.Join(strings.Fields(label), " ")
	for _, article := range []string{"the ", "my ", "our ", "another ", "a ", "an "} {
		label = strings.TrimPrefix(label, article)
	}
	label = strings.TrimSuffix(label, " timer")
	if label == "" || labelStopwords[label] || looksLikeDuration(label) {
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
