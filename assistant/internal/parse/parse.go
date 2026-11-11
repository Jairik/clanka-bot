// Package parse reads simple spoken numbers and durations without a model.
package parse

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// units maps an explicit unit word to its duration.
var units = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
}

// smallNumbers covers the number words a short utterance uses.
var smallNumbers = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

var (
	digitLetter = regexp.MustCompile(`([0-9])([a-z])`)
	letterDigit = regexp.MustCompile(`([a-z])([0-9])`)
	clockColon  = regexp.MustCompile(`([0-9]{1,2}):([0-9]{2})`)
	nonAlnum    = regexp.MustCompile(`[^a-z0-9]+`)
)

// Duration returns the first duration-like span in text. A bare number at the
// end of the utterance uses defaultUnit; zero disables that fallback.
func Duration(text string, defaultUnit time.Duration) (time.Duration, bool) {
	tokens := tokenize(text)
	// Prefer a span with an explicit unit so filler words ("a timer") are ignored.
	for i := range tokens {
		if d, end, ok := parseAt(tokens, i, 0); ok && end > i {
			return d, true
		}
	}
	// A bare number is a duration only when it ends the utterance.
	if defaultUnit > 0 {
		for i := len(tokens) - 1; i >= 0; i-- {
			if d, end, ok := parseAt(tokens, i, defaultUnit); ok && end == len(tokens) && end > i {
				return d, true
			}
		}
	}
	return 0, false
}

// Number returns the first number-like span in text.
func Number(text string) (int, bool) {
	tokens := tokenize(text)
	for i := 0; i < len(tokens); i++ {
		if tokens[i] == "a" || tokens[i] == "an" {
			continue
		}
		if n, _, ok := numberAt(tokens, i); ok {
			return n, true
		}
	}
	return 0, false
}

// HumanDuration renders a duration the way Speak replies say it.
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	total := int(d.Round(time.Second) / time.Second)
	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	var parts []string
	if hours > 0 {
		parts = append(parts, plural(hours, "hour"))
	}
	if minutes > 0 {
		parts = append(parts, plural(minutes, "minute"))
	}
	if seconds > 0 {
		parts = append(parts, plural(seconds, "second"))
	}
	return strings.Join(parts, " ")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// tokenize lowercases text, splits clock notation ("1:30") and glued units
// ("1h30m"), then splits on everything that is not a letter or digit.
func tokenize(text string) []string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = clockColon.ReplaceAllString(s, "$1 h $2 m")
	s = strings.ReplaceAll(s, "-", " ")
	for {
		next := digitLetter.ReplaceAllString(s, "$1 $2")
		next = letterDigit.ReplaceAllString(next, "$1 $2")
		if next == s {
			break
		}
		s = next
	}
	return nonAlnum.Split(s, -1)
}

// parseAt parses one duration starting at tokens[start]. It returns the
// duration and the index just past the last consumed token.
func parseAt(tokens []string, start int, defaultUnit time.Duration) (time.Duration, int, bool) {
	var total time.Duration
	var lastUnit time.Duration
	explicitUnit := false
	pieces := 0
	i := start
	for i < len(tokens) {
		tok := tokens[i]
		switch tok {
		case "and":
			if pieces == 0 {
				return 0, i, false
			}
			i++
			continue
		case "half", "quarter":
			divisor := 2
			if tok == "quarter" {
				divisor = 4
			}
			j := i + 1
			if j < len(tokens) && (tokens[j] == "a" || tokens[j] == "an") {
				j++
			}
			if j < len(tokens) {
				if u, ok := units[tokens[j]]; ok {
					total += u / time.Duration(divisor)
					lastUnit = u
					explicitUnit = true
					i = j + 1
					pieces++
					continue
				}
			}
			if lastUnit != 0 {
				total += lastUnit / time.Duration(divisor)
				i++
				pieces++
				continue
			}
			return 0, i, false
		}

		// "an hour and a half": the "and" is skipped above, so "a half" folds
		// into the previous unit's fraction.
		if (tok == "a" || tok == "an") && i+1 < len(tokens) && (tokens[i+1] == "half" || tokens[i+1] == "quarter") {
			if lastUnit == 0 {
				return 0, i, false
			}
			divisor := 2
			if tokens[i+1] == "quarter" {
				divisor = 4
			}
			total += lastUnit / time.Duration(divisor)
			i += 2
			pieces++
			continue
		}

		n, next, ok := numberAt(tokens, i)
		if !ok {
			break
		}

		// "one and a half hours" carries its unit after the fraction.
		if next+2 < len(tokens) && tokens[next] == "and" && (tokens[next+1] == "a" || tokens[next+1] == "an") && tokens[next+2] == "half" {
			u := lastUnit
			end := next + 3
			if end < len(tokens) {
				if uu, ok := units[tokens[end]]; ok {
					u = uu
					end++
				}
			}
			if u == 0 {
				return 0, i, false
			}
			total += time.Duration(n)*u + u/2
			lastUnit = u
			explicitUnit = true
			i = end
			pieces++
			continue
		}

		if next < len(tokens) {
			if u, ok := units[tokens[next]]; ok {
				total += time.Duration(n) * u
				lastUnit = u
				explicitUnit = true
				i = next + 1
				pieces++
				continue
			}
		}

		// "1 h 30": a trailing number after a unit uses the next smaller unit.
		if lastUnit != 0 {
			u := smallerUnit(lastUnit)
			total += time.Duration(n) * u
			lastUnit = u
			i = next
			pieces++
			continue
		}

		// A bare number is valid only at the end of the utterance.
		if defaultUnit > 0 && next == len(tokens) {
			total += time.Duration(n) * defaultUnit
			lastUnit = defaultUnit
			i = next
			pieces++
			continue
		}
		return 0, i, false
	}
	if pieces == 0 || total <= 0 {
		return 0, i, false
	}
	if !explicitUnit && defaultUnit == 0 {
		return 0, i, false
	}
	return total, i, true
}

func smallerUnit(unit time.Duration) time.Duration {
	switch unit {
	case time.Hour:
		return time.Minute
	case time.Minute:
		return time.Second
	default:
		return time.Second
	}
}

// numberAt reads a digit run or a run of number words.
func numberAt(tokens []string, i int) (int, int, bool) {
	if i >= len(tokens) {
		return 0, i, false
	}
	tok := tokens[i]
	if isDigits(tok) {
		n, err := strconv.Atoi(tok)
		if err != nil {
			return 0, i, false
		}
		return n, i + 1, true
	}
	if tok == "a" || tok == "an" {
		return 1, i + 1, true
	}
	if _, ok := smallNumbers[tok]; !ok && tok != "hundred" {
		return 0, i, false
	}
	j := i
	var words []string
	for j < len(tokens) {
		if _, ok := smallNumbers[tokens[j]]; ok || tokens[j] == "hundred" {
			words = append(words, tokens[j])
			j++
			continue
		}
		break
	}
	n, ok := wordsToNumber(words)
	if !ok {
		return 0, i, false
	}
	return n, j, true
}

func wordsToNumber(words []string) (int, bool) {
	current := 0
	lastTens := false
	for _, w := range words {
		if w == "hundred" {
			if current == 0 {
				current = 1
			}
			current *= 100
			lastTens = false
			continue
		}
		n := smallNumbers[w]
		if n >= 20 {
			if lastTens {
				return 0, false
			}
			current += n
			lastTens = true
			continue
		}
		if lastTens {
			lastTens = false
		} else if current%10 != 0 {
			return 0, false
		}
		current += n
	}
	return current, current > 0
}

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
