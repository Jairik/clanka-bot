// Package random implements random.flip, random.roll and random.pick. Physical
// randomness comes from crypto/rand through one Intn seam, so tests are
// deterministic and production is not seeded from the clock.
package random

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	FlipName = "random.flip"
	RollName = "random.roll"
	PickName = "random.pick"
)

const (
	maxDice          = 100
	minSides         = 2
	maxSides         = 1000
	maxPickOptions   = 50
	maxOptionLength  = 100
	maxRandomRetries = 3
)

// Intn returns an unbiased integer in [0,n). Production uses crypto/rand; tests
// inject a fixed sequence through NewModuleWithSource.
type Intn func(n int) (int, error)

// cryptoIntn draws one uniform value without modulo bias.
func cryptoIntn(n int) (int, error) {
	if n <= 0 || int64(n) > math.MaxInt32 {
		return 0, errors.New("random: bound must be a positive int32")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("random: read: %w", err)
	}
	return int(value.Int64()), nil
}

// FlipIn has no arguments.
type FlipIn struct{}

// FlipOut is the side that landed up.
type FlipOut struct {
	Side string `json:"side"`
}

// RollIn is the number of dice and their sides. Zero values use one d6.
type RollIn struct {
	Count int `json:"count,omitempty"`
	Sides int `json:"sides,omitempty"`
}

// RollOut keeps every die so a caller can show the individual results.
type RollOut struct {
	Count int   `json:"count"`
	Sides int   `json:"sides"`
	Rolls []int `json:"rolls"`
	Total int   `json:"total"`
}

// PickIn is the list of options as the user said them.
type PickIn struct {
	Options []string `json:"options"`
}

// PickOut is the one chosen option and the list it came from.
type PickOut struct {
	Options []string `json:"options"`
	Choice  string   `json:"choice"`
	Index   int      `json:"index"`
}

// NewFlip builds random.flip. A nil source uses crypto/rand.
func NewFlip(intn Intn) tool.Tool[FlipIn, FlipOut] {
	intn = sourceOrCrypto(intn)
	return tool.Tool[FlipIn, FlipOut]{
		Name:        FlipName,
		Description: "Flip a fair coin.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, FlipIn) (FlipOut, error) {
			index, err := draw(intn, 2)
			if err != nil {
				return FlipOut{}, &tool.ToolError{Say: "I couldn't flip a coin."}
			}
			if index == 0 {
				return FlipOut{Side: "heads"}, nil
			}
			return FlipOut{Side: "tails"}, nil
		},
		Speak: func(out FlipOut) string {
			if out.Side == "heads" {
				return "Heads."
			}
			return "Tails."
		},
		Examples: []string{
			"flip a coin",
			"toss a coin",
			"heads or tails",
			"flip a coin for me",
			"coin flip",
			"let's flip a coin",
			"flip it",
			"toss it",
		},
	}
}

// NewRoll builds random.roll. The sides are inclusive: a d20 yields 1 through
// 20.
func NewRoll(intn Intn) tool.Tool[RollIn, RollOut] {
	intn = sourceOrCrypto(intn)
	return tool.Tool[RollIn, RollOut]{
		Name:        RollName,
		Description: "Roll one or more fair dice and report each result and the total.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in RollIn) (RollOut, error) {
			count, sides := in.Count, in.Sides
			if count == 0 {
				count = 1
			}
			if sides == 0 {
				sides = 6
			}
			if count < 1 || count > maxDice || sides < minSides || sides > maxSides {
				return RollOut{}, &tool.ToolError{Say: fmt.Sprintf(
					"I can roll 1 to %d dice, with between %d and %d sides.", maxDice, minSides, maxSides)}
			}
			rolls := make([]int, 0, count)
			total := 0
			for i := 0; i < count; i++ {
				value, err := draw(intn, sides)
				if err != nil {
					return RollOut{}, &tool.ToolError{Say: "I couldn't roll the dice."}
				}
				rolls = append(rolls, value+1)
				total += value + 1
			}
			return RollOut{Count: count, Sides: sides, Rolls: rolls, Total: total}, nil
		},
		Speak: func(out RollOut) string {
			if out.Count == 1 {
				return fmt.Sprintf("You rolled %d.", out.Total)
			}
			return fmt.Sprintf("You rolled %d dice for a total of %d.", out.Count, out.Total)
		},
		Extract: extractRoll,
		Examples: []string{
			"roll a d20",
			"roll a die",
			"roll two dice",
			"roll 2d6",
			"roll a six sided die",
			"roll 3 dice",
			"roll a twenty sided die",
		},
	}
}

// NewPick builds random.pick. The function preserves the user's words and only
// trims whitespace and surrounding punctuation.
func NewPick(intn Intn) tool.Tool[PickIn, PickOut] {
	intn = sourceOrCrypto(intn)
	return tool.Tool[PickIn, PickOut]{
		Name:        PickName,
		Description: "Pick one option from a list the user gave, without changing the words.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in PickIn) (PickOut, error) {
			options := cleanOptions(in.Options)
			if len(options) == 0 {
				return PickOut{}, &tool.ToolError{Say: "Tell me which options to pick between."}
			}
			if len(options) > maxPickOptions {
				return PickOut{}, &tool.ToolError{Say: fmt.Sprintf("I can pick from up to %d options.", maxPickOptions)}
			}
			for _, option := range options {
				if len(option) > maxOptionLength {
					return PickOut{}, &tool.ToolError{Say: "That option is too long for me to say back."}
				}
			}
			index, err := draw(intn, len(options))
			if err != nil {
				return PickOut{}, &tool.ToolError{Say: "I couldn't pick an option."}
			}
			return PickOut{Options: options, Choice: options[index], Index: index}, nil
		},
		Speak: func(out PickOut) string {
			return "I'd pick " + out.Choice + "."
		},
		Extract: extractPick,
		Examples: []string{
			"pick between pizza and tacos",
			"choose between tea and coffee",
			"pick from pizza, tacos or sushi",
			"pick one: red or blue",
			"randomly pick between the red one and the blue one",
			"help me choose between a cat and a dog",
			// The option words change with every request; these examples keep
			// the choose-between shape in the index across common categories.
			"pick between the beach and the mountains",
			"choose between the train and the bus",
			"pick from chicken, fish or vegetables",
			"pick between hiking and swimming",
			"choose between the movie and the concert",
			"choose a movie or a book",
			"pick one of pasta or salad",
			"pick from walk or drive",
		},
	}
}

// Module is the random Tool domain. It has no settings.
type Module struct {
	intn Intn
}

// NewModule builds the random module with crypto/rand.
func NewModule() *Module { return &Module{intn: cryptoIntn} }

// NewModuleWithSource builds the module with an injected randomness seam.
func NewModuleWithSource(intn Intn) *Module { return &Module{intn: sourceOrCrypto(intn)} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "random" }

// Register adds the three random Tools.
func (m *Module) Register(r *tool.Registry, _ tool.Deps) error {
	for _, add := range []func() error{
		func() error { return tool.Add(r, NewFlip(m.intn)) },
		func() error { return tool.Add(r, NewRoll(m.intn)) },
		func() error { return tool.Add(r, NewPick(m.intn)) },
	} {
		if err := add(); err != nil {
			return err
		}
	}
	return nil
}

// sourceOrCrypto replaces a nil seam with crypto/rand.
func sourceOrCrypto(intn Intn) Intn {
	if intn == nil {
		return cryptoIntn
	}
	return intn
}

// draw calls the seam and validates its result. A misbehaving injected source
// is an error rather than a panic or an out-of-range outcome.
func draw(intn Intn, n int) (int, error) {
	for attempt := 0; attempt < maxRandomRetries; attempt++ {
		value, err := intn(n)
		if err != nil {
			return 0, err
		}
		if value >= 0 && value < n {
			return value, nil
		}
	}
	return 0, errors.New("random: source returned an out-of-range value")
}

// dicePattern matches d20 and 2d6, including spaced forms.
var dicePattern = regexp.MustCompile(`\b(?:([0-9]{1,3})\s*)?d\s*([0-9]{1,4})\b`)

// sidedPattern matches "six sided" and "20 sided" after hyphen normalisation.
var sidedPattern = regexp.MustCompile(`\b([0-9]{1,3}|[a-z]+)\s+sided\b`)

// countDicePattern matches "3 dice" or "two dice".
var countDicePattern = regexp.MustCompile(`\b([0-9]{1,3}|[a-z]+)\s+(?:fair\s+)?dice\b`)

// sidesPattern matches "with eight sides".
var sidesPattern = regexp.MustCompile(`\b([0-9]{1,3}|[a-z]+)\s+sides\b`)

// extractRoll reads the common dice phrasings and leaves bounds to the
// handler, so a huge request gets a helpful limit message.
func extractRoll(text string) (RollIn, bool) {
	raw := strings.ToLower(strings.TrimSpace(text))
	raw = strings.Trim(raw, " \t\r\n?!.,")
	raw = strings.ReplaceAll(raw, "-", " ")
	if !strings.Contains(raw, "dice") && !strings.Contains(raw, "die") &&
		!strings.Contains(raw, "sided") && !dicePattern.MatchString(raw) {
		return RollIn{}, false
	}
	// "roll out the red carpet" is not a dice request.
	if !strings.Contains(raw, "roll") && !strings.Contains(raw, "dice") && !strings.Contains(raw, "die") {
		return RollIn{}, false
	}

	count, sides := 0, 0
	if strings.Contains(raw, "pair of dice") {
		count = 2
	}
	if match := dicePattern.FindStringSubmatchIndex(raw); match != nil {
		sides = numberWord(raw[match[4]:match[5]])
		if match[2] >= 0 {
			count = numberWord(raw[match[2]:match[3]])
		} else {
			count = countBefore(raw[:match[0]])
		}
	} else if match := sidedPattern.FindStringSubmatchIndex(raw); match != nil {
		sides = numberWord(raw[match[2]:match[3]])
		count = countBefore(raw[:match[0]])
	} else if match := countDicePattern.FindStringSubmatchIndex(raw); match != nil {
		if parsed := numberWord(raw[match[2]:match[3]]); parsed > 0 {
			count = parsed
		}
		if sideMatch := sidesPattern.FindStringSubmatchIndex(raw); sideMatch != nil {
			sides = numberWord(raw[sideMatch[2]:sideMatch[3]])
		}
	}
	if count == 0 {
		count = 1
	}
	if sides == 0 {
		sides = 6
	}
	return RollIn{Count: count, Sides: sides}, true
}

// countBefore returns the last number-like word before a dice phrase. Articles
// are ignored so "roll a six sided die" is one die.
func countBefore(prefix string) int {
	fields := strings.Fields(prefix)
	for i := len(fields) - 1; i >= 0; i-- {
		word := strings.Trim(fields[i], "-")
		if word == "a" || word == "an" || word == "the" || word == "roll" {
			continue
		}
		if value, ok := parseNumber(word); ok {
			return value
		}
		break
	}
	return 0
}

// numberWord parses one dice count or side word.
func numberWord(word string) int {
	if value, ok := parseNumber(strings.ToLower(word)); ok {
		return value
	}
	return 0
}

// parseNumber accepts digits and the small spoken numbers parse already knows.
func parseNumber(word string) (int, bool) {
	if value, err := strconv.Atoi(word); err == nil {
		return value, true
	}
	return parse.Number(word)
}

// extractPick reads a list only when the user supplied at least two options.
// It never invents an option or maps a word to a category.
func extractPick(text string) (PickIn, bool) {
	raw := strings.TrimSpace(text)
	raw = strings.Trim(raw, " \t\r\n?!.,")
	lower := strings.ToLower(raw)
	for _, prefix := range []string{
		"please ", "can you ", "could you ", "would you ", "help me ",
		"randomly ", "random ",
	} {
		if strings.HasPrefix(lower, prefix) {
			raw = strings.TrimSpace(raw[len(prefix):])
			lower = strings.ToLower(raw)
			break
		}
	}
	start := -1
	for _, verb := range []string{"pick", "choose", "select"} {
		if strings.HasPrefix(lower, verb+" ") || strings.HasPrefix(lower, verb+":") {
			start = len(verb)
			break
		}
	}
	if start < 0 {
		return PickIn{}, false
	}
	rest := strings.TrimSpace(raw[start:])
	rest = strings.TrimLeft(rest, ":")
	rest = strings.TrimSpace(rest)
	lowerRest := strings.ToLower(rest)
	// A numeric range reads like a pick but is arithmetic, not a list.
	if strings.Contains(lowerRest, "number between ") || strings.Contains(lowerRest, "number from ") {
		return PickIn{}, false
	}
	if strings.HasPrefix(lowerRest, "one of ") {
		rest = rest[len("one of "):]
	} else if strings.HasPrefix(lowerRest, "one:") {
		rest = rest[len("one:"):]
	}
	rest = strings.TrimSpace(rest)
	lowerRest = strings.ToLower(rest)
	if strings.HasPrefix(lowerRest, "between ") {
		rest = rest[len("between "):]
	} else if strings.HasPrefix(lowerRest, "from ") {
		rest = rest[len("from "):]
	}
	options := splitOptions(rest)
	if len(options) < 2 {
		return PickIn{}, false
	}
	// "pick a number between 1 and 10" asks for a range, not two labels.
	if allNumbers(options) {
		return PickIn{}, false
	}
	return PickIn{Options: options}, true
}

// splitOptions splits on commas, then " or ", then " and ". The user's words
// and their order are preserved.
func splitOptions(rest string) []string {
	var parts []string
	if strings.Contains(rest, ",") {
		for _, part := range strings.Split(rest, ",") {
			part = strings.TrimSpace(part)
			lower := strings.ToLower(part)
			for _, prefix := range []string{"and ", "or "} {
				if strings.HasPrefix(lower, prefix) {
					part = part[len(prefix):]
					break
				}
			}
			// "chicken, fish or vegetables" is three options, not two.
			if strings.Contains(strings.ToLower(part), " or ") {
				parts = append(parts, splitOnFold(part, " or ")...)
			} else {
				parts = append(parts, part)
			}
		}
	} else if strings.Contains(strings.ToLower(rest), " or ") {
		parts = splitOnFold(rest, " or ")
	} else if strings.Contains(strings.ToLower(rest), " and ") {
		parts = splitOnFold(rest, " and ")
	}
	var options []string
	for _, part := range parts {
		if option := cleanOption(part); option != "" {
			options = append(options, option)
		}
	}
	return options
}

// splitOnFold splits on a lowercase separator while keeping the original text.
func splitOnFold(text, separator string) []string {
	lower := strings.ToLower(text)
	var parts []string
	for {
		index := strings.Index(lower, separator)
		if index < 0 {
			parts = append(parts, text)
			return parts
		}
		parts = append(parts, text[:index])
		text = text[index+len(separator):]
		lower = lower[index+len(separator):]
	}
}

// cleanOption trims surrounding punctuation only.
func cleanOption(option string) string {
	option = strings.TrimSpace(option)
	option = strings.Trim(option, "\"'")
	option = strings.TrimSpace(option)
	option = strings.TrimRight(option, ".!?")
	return strings.TrimSpace(option)
}

// cleanOptions copies and trims a caller-supplied list.
func cleanOptions(options []string) []string {
	cleaned := make([]string, 0, len(options))
	for _, option := range options {
		if option = cleanOption(option); option != "" {
			cleaned = append(cleaned, option)
		}
	}
	return cleaned
}

// allNumbers reports whether every option is a plain integer.
func allNumbers(options []string) bool {
	for _, option := range options {
		if _, err := strconv.Atoi(option); err != nil {
			return false
		}
	}
	return true
}
