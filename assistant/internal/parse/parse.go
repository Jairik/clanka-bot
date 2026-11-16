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

// smallNumbers covers the number words a short utterance uses. The spoken zero
// forms ("oh", "o", "nought") are included so a fraction word a user actually
// says is readable; see valueAt for how a leading zero keeps its digit.
var smallNumbers = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
	"oh": 0, "o": 0, "nought": 0,
}

var (
	digitLetter = regexp.MustCompile(`([0-9])([a-z])`)
	letterDigit = regexp.MustCompile(`([a-z])([0-9])`)
	clockColon  = regexp.MustCompile(`([0-9]{1,2}):([0-9]{2})`)
	// decimalUnit detaches a unit glued to a decimal before the digit/letter
	// pass, so "1.75h" becomes "1.75 h" rather than "1.7 5 h".
	decimalUnit = regexp.MustCompile(`([0-9]*\.[0-9]+)([a-z])`)
	// decimalOrWord keeps a decimal one token; every other rune is a break.
	decimalOrWord = regexp.MustCompile(`[0-9]*\.[0-9]+|[a-z0-9]+`)
)

// maxDuration bounds one parsed duration so an absurd spoken number falls
// through to the Agent path instead of wrapping into a smaller duration.
const maxDuration = time.Duration(1<<63 - 1)

// scaleDivisor is the divisor for one or two fractional digits.
var scaleDivisor = [3]time.Duration{0, 10, 100}

// Duration returns the first duration-like span in text. A written decimal
// ("1.5") and the spoken point form ("one point five") read as that number. A
// bare number at the end of the utterance uses defaultUnit; zero disables that
// fallback.
func Duration(text string, defaultUnit time.Duration) (time.Duration, bool) {
	tokens := tokenize(text)
	// Prefer a span with an explicit unit so filler words ("a timer") are ignored.
	for i := range tokens {
		if d, end, ok := parseAt(tokens, i, 0); ok && end > i {
			return d, true
		}
	}
	// A bare number is a duration only when it ends the utterance. The scan
	// runs forward so the earliest number run wins, keeping "one point five"
	// from being read as the trailing "five".
	if defaultUnit > 0 {
		for i := range tokens {
			if d, end, ok := parseAt(tokens, i, defaultUnit); ok && end == len(tokens) && end > i {
				return d, true
			}
		}
	}
	return 0, false
}

// Number returns the first number-like span in text. A decimal keeps its
// whole part ("40.5" reads as 40), matching the volume-style callers.
func Number(text string) (int, bool) {
	tokens := tokenize(text)
	for i := 0; i < len(tokens); i++ {
		if tokens[i] == "a" || tokens[i] == "an" {
			continue
		}
		if whole, _, _, _, ok := valueAt(tokens, i); ok {
			return whole, true
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
// ("1h30m"), then keeps decimals ("1.5") whole and drops everything else.
func tokenize(text string) []string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = clockColon.ReplaceAllString(s, "$1 h $2 m")
	s = strings.ReplaceAll(s, "-", " ")
	s = decimalUnit.ReplaceAllString(s, "$1 $2")
	for {
		next := digitLetter.ReplaceAllString(s, "$1 $2")
		next = letterDigit.ReplaceAllString(next, "$1 $2")
		if next == s {
			break
		}
		s = next
	}
	return decimalOrWord.FindAllString(s, -1)
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
					if !addPiece(&total, u/time.Duration(divisor)) {
						return 0, i, false
					}
					lastUnit = u
					explicitUnit = true
					i = j + 1
					pieces++
					continue
				}
			}
			if lastUnit != 0 {
				if !addPiece(&total, lastUnit/time.Duration(divisor)) {
					return 0, i, false
				}
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
			if !addPiece(&total, lastUnit/time.Duration(divisor)) {
				return 0, i, false
			}
			i += 2
			pieces++
			continue
		}

		whole, frac, fracDigits, next, ok := valueAt(tokens, i)
		if !ok {
			break
		}

		// "one and a half hours" carries its unit after the fraction.
		if fracDigits == 0 && next+2 < len(tokens) && tokens[next] == "and" && (tokens[next+1] == "a" || tokens[next+1] == "an") && tokens[next+2] == "half" {
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
			piece, ok := unitPiece(whole, 0, 0, u)
			if !ok || !addPiece(&total, piece) || !addPiece(&total, u/2) {
				return 0, i, false
			}
			lastUnit = u
			explicitUnit = true
			i = end
			pieces++
			continue
		}

		if next < len(tokens) {
			if u, ok := units[tokens[next]]; ok {
				piece, ok := unitPiece(whole, frac, fracDigits, u)
				if !ok || !addPiece(&total, piece) {
					return 0, i, false
				}
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
			piece, ok := unitPiece(whole, frac, fracDigits, u)
			if !ok || !addPiece(&total, piece) {
				return 0, i, false
			}
			lastUnit = u
			i = next
			pieces++
			continue
		}

		// A bare number is valid only at the end of the utterance.
		if defaultUnit > 0 && next == len(tokens) {
			piece, ok := unitPiece(whole, frac, fracDigits, defaultUnit)
			if !ok || !addPiece(&total, piece) {
				return 0, i, false
			}
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

// valueAt reads one number: a digit run, a decimal ("1.5", ".5"), a number
// word run, or a number word followed by a spoken "point" fraction. It returns
// the whole part, the fraction, the fraction's digit count and the index just
// past the number.
func valueAt(tokens []string, i int) (whole, frac, fracDigits, next int, ok bool) {
	if i >= len(tokens) {
		return 0, 0, 0, i, false
	}
	if strings.IndexByte(tokens[i], '.') >= 0 {
		whole, frac, fracDigits, ok = parseDecimal(tokens[i])
		if !ok {
			return 0, 0, 0, i, false
		}
		return whole, frac, fracDigits, i + 1, true
	}
	whole, next, ok = numberAt(tokens, i)
	if !ok {
		return 0, 0, 0, i, false
	}
	if next < len(tokens) && tokens[next] == "point" {
		frac, after, fine := fractionAt(tokens, next+1)
		if !fine {
			return 0, 0, 0, i, false
		}
		// The fraction's digit count is not just decimalDigits(frac): a leading
		// zero word ("oh", "o", "nought") holds a digit position, so "one point
		// oh five" is 1.05 (two fraction digits), never 1.5. Count the words
		// after "point" when that is more than the integer's digit count.
		fracDigits := decimalDigits(frac)
		if n := after - (next + 1); n > fracDigits {
			fracDigits = n
		}
		return whole, frac, fracDigits, after, true
	}
	return whole, 0, 0, next, true
}

// parseDecimal splits a written decimal such as "1.5" or ".5". One or two
// fraction digits are supported; a longer fraction is rejected rather than
// rounded or truncated to a different number.
func parseDecimal(token string) (whole, frac, digits int, ok bool) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return 0, 0, 0, false
	}
	wholeText, fracText := token[:dot], token[dot+1:]
	if len(fracText) > 2 || !isDigits(fracText) {
		return 0, 0, 0, false
	}
	if wholeText == "" {
		wholeText = "0"
	}
	if !isDigits(wholeText) {
		return 0, 0, 0, false
	}
	whole, err := strconv.Atoi(wholeText)
	if err != nil {
		return 0, 0, 0, false
	}
	frac, err = strconv.Atoi(fracText)
	if err != nil {
		return 0, 0, 0, false
	}
	return whole, frac, len(fracText), true
}

// fractionAt reads the number after a spoken "point". The article forms are
// refused so "one point a" is not read as one and a tenth.
func fractionAt(tokens []string, i int) (int, int, bool) {
	if i >= len(tokens) || tokens[i] == "a" || tokens[i] == "an" {
		return 0, i, false
	}
	return numberAt(tokens, i)
}

// decimalDigits counts the fraction digits a spoken value stands for, so a
// "five" is five tenths and a "twenty five" is twenty-five hundredths.
func decimalDigits(fraction int) int {
	if fraction == 0 {
		return 1
	}
	return len(strconv.Itoa(fraction))
}

// unitPiece converts a whole count plus an optional fractional count of unit
// into a duration. The arithmetic is exact, so "1.5 hours" is 90 minutes and
// never a rounded count of whole units.
func unitPiece(whole, frac, fracDigits int, unit time.Duration) (time.Duration, bool) {
	if unit <= 0 || whole < 0 || frac < 0 || fracDigits < 0 || fracDigits > 2 {
		return 0, false
	}
	if int64(whole) > int64(maxDuration/unit) {
		return 0, false
	}
	piece := time.Duration(whole) * unit
	if fracDigits > 0 {
		// A fraction of a supported unit is exact: a tenth of an hour is six
		// minutes, and frac stays below a hundred.
		piece += time.Duration(frac) * unit / scaleDivisor[fracDigits]
	}
	return piece, true
}

// addPiece adds one scaled piece to the running total, rejecting overflow.
func addPiece(total *time.Duration, piece time.Duration) bool {
	if piece < 0 || piece > maxDuration-*total {
		return false
	}
	*total += piece
	return true
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
	if !isNumberWord(tok) {
		return 0, i, false
	}
	j := i
	var words []string
	for j < len(tokens) {
		if isNumberWord(tokens[j]) {
			words = append(words, tokens[j])
			j++
			continue
		}
		// "one hundred and twenty" is one number, but "one and a half" is not:
		// the conjunction belongs to the number only when a number word follows.
		if tokens[j] == "and" && j+1 < len(tokens) && isNumberWord(tokens[j+1]) {
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

// isNumberWord reports whether one token names a whole number part.
func isNumberWord(token string) bool {
	if _, ok := smallNumbers[token]; ok {
		return true
	}
	return token == "hundred"
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
	return current, current > 0 || len(words) > 0
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

// labelCap is the most runes a spoken timer/alarm label may hold. Truncating on
// bytes would split a multi-byte rune, so callers cap with CapLabel instead of
// slicing the string directly.
const labelCap = 40

// CapLabel trims label to at most n runes so a multi-byte rune is never split
// mid-character. The result is also space-trimmed, matching the cleaner's prior
// behaviour. Inputs already at or below n runes are unchanged.
func CapLabel(label string, n int) string {
	r := []rune(label)
	if len(r) <= n {
		return label
	}
	return strings.TrimSpace(string(r[:n]))
}

// LabelCap returns the maximum runes a spoken label may hold.
func LabelCap() int {
	return labelCap
}
