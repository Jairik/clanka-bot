// Package calc implements calc.evaluate with a small arithmetic grammar. It
// parses numbers, + - * /, parentheses and a postfix percent; it never
// evaluates Go code or calls out to a calculator process.
package calc

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical calc.evaluate Tool name.
const Name = "calc.evaluate"

const (
	// maxExpression keeps one utterance from turning into a long parse. The
	// parser limits below bound work; this limit bounds the bytes copied.
	maxExpression = 200
	maxTokens     = 128
	maxDepth      = 16
	maxOperations = 64
	// maxResult keeps a finite float from becoming an unreadable number. The
	// user asked for arithmetic, not arbitrary precision.
	maxResult = 1e15
)

// In is one arithmetic expression as spoken or typed.
type In struct {
	Expression string `json:"expression"`
}

// Out is the evaluated expression. Value is the number a caller can use;
// Display is the trimmed form Speak says.
type Out struct {
	Expression string  `json:"expression"`
	Value      float64 `json:"value"`
	Display    string  `json:"display"`
}

// New builds the calc.evaluate Tool.
func New() tool.Tool[In, Out] {
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Evaluate a safe arithmetic expression, including a percentage of a number.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in In) (Out, error) {
			expression := strings.TrimSpace(in.Expression)
			if expression == "" {
				return Out{}, &tool.ToolError{Say: "Tell me the calculation to work out."}
			}
			if len(expression) > maxExpression {
				return Out{}, &tool.ToolError{Say: "That calculation is too long."}
			}
			value, err := Evaluate(expression)
			if err != nil {
				return Out{}, &tool.ToolError{Say: err.Error()}
			}
			return Out{Expression: expression, Value: value, Display: formatNumber(value)}, nil
		},
		Speak: func(out Out) string {
			return out.Expression + " is " + out.Display + "."
		},
		Examples: []string{
			"what is 15% of 80",
			"what's 17 times 24",
			"calculate 12 plus 7",
			"how much is 100 divided by 4",
			"what is 2 plus 2",
			"work out 8 times 9",
			"what's 20 percent of 150",
			"compute 3 plus 4 times 2",
			// The operator words are the routing signal, not the numbers.
			"what is 50 minus 18",
			"what is 45 divided by 5",
			"what is 3 x 4",
			"what's 5 times 12",
			"what is 10 percent of 90",
			"what is 99 plus 1",
			"calculate 6 plus 6",
		},
		Extract: extract,
	}
}

// Evaluate parses and evaluates one expression. It accepts only the documented
// grammar, so text such as a shell command or a function call is a parse error
// rather than something to run.
func Evaluate(expression string) (float64, error) {
	tokens, err := lex(expression)
	if err != nil {
		return 0, err
	}
	if len(tokens) == 0 {
		return 0, errors.New("I couldn't read that as arithmetic.")
	}
	p := parser{tokens: tokens}
	value, err := p.parseExpression()
	if err != nil {
		return 0, err
	}
	if p.pos != len(p.tokens) {
		return 0, errors.New("I couldn't read that as arithmetic.")
	}
	return value, nil
}

// extract accepts only an expression that parses. A request such as "what's the
// square root of 144" therefore stays on the Agent path instead of becoming a
// Fast path failure.
func extract(text string) (In, bool) {
	expression, ok := expressionFrom(text)
	if !ok {
		return In{}, false
	}
	if _, err := Evaluate(expression); err != nil {
		return In{}, false
	}
	return In{Expression: expression}, true
}

// expressionFrom strips the spoken request wrapper and leaves the arithmetic.
func expressionFrom(text string) (string, bool) {
	expression := strings.ToLower(strings.TrimSpace(text))
	expression = strings.Trim(expression, " \t\r\n?!.")
	prefixes := []string{
		"please ", "hey ", "so ", "can you ", "could you ", "would you ", "will you ",
		"what is ", "what's ", "whats ", "what are ", "how much is that ", "how much is ",
		"calculate ", "compute ", "work out ", "evaluate ", "solve ", "tell me ", "give me ",
	}
	for {
		trimmed := strings.TrimSpace(expression)
		changed := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				trimmed = strings.TrimSpace(trimmed[len(prefix):])
				changed = true
				break
			}
		}
		if !changed {
			expression = trimmed
			break
		}
		expression = trimmed
	}
	for _, suffix := range []string{" for me", " please"} {
		expression = strings.TrimSpace(strings.TrimSuffix(expression, suffix))
	}
	if expression == "" || !containsDigit(expression) {
		return "", false
	}
	return expression, true
}

// tokenKind is one accepted token. The grammar is a whitelist: no identifiers,
// no function names and no statements.
type tokenKind uint8

const (
	tokenNumber tokenKind = iota + 1
	tokenPlus
	tokenMinus
	tokenStar
	tokenSlash
	tokenPercent
	tokenLParen
	tokenRParen
	tokenOf
)

type token struct {
	kind  tokenKind
	value float64
}

// lex turns the expression into tokens, accepting digits, symbols and the
// spoken operator words. Commas are ignored so "1,000 + 2" reads naturally.
func lex(expression string) ([]token, error) {
	var tokens []token
	runes := []rune(expression)
	// A thousands separator sits between digits; a comma elsewhere stays a
	// harmless separator.
	cleaned := runes[:0]
	for i, r := range runes {
		if r == ',' && i > 0 && i+1 < len(runes) && isDigit(runes[i-1]) && isDigit(runes[i+1]) {
			continue
		}
		cleaned = append(cleaned, r)
	}
	runes = cleaned
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case unicode.IsSpace(r) || r == ',':
			i++
		case r >= '0' && r <= '9' || r == '.' && i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9':
			start := i
			dots := 0
			for i < len(runes) && (runes[i] >= '0' && runes[i] <= '9' || runes[i] == '.') {
				if runes[i] == '.' {
					dots++
				}
				i++
			}
			if dots > 1 || i-start > 24 {
				return nil, errors.New("I couldn't read that as arithmetic.")
			}
			value, err := strconv.ParseFloat(string(runes[start:i]), 64)
			if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || math.Abs(value) > maxResult {
				return nil, errors.New("That number is too large.")
			}
			tokens = append(tokens, token{kind: tokenNumber, value: value})
		case unicode.IsLetter(r):
			start := i
			for i < len(runes) && unicode.IsLetter(runes[i]) {
				i++
			}
			word := strings.ToLower(string(runes[start:i]))
			switch word {
			case "plus":
				tokens = append(tokens, token{kind: tokenPlus})
			case "minus":
				tokens = append(tokens, token{kind: tokenMinus})
			case "times", "x":
				tokens = append(tokens, token{kind: tokenStar})
			case "over":
				tokens = append(tokens, token{kind: tokenSlash})
			case "percent":
				tokens = append(tokens, token{kind: tokenPercent})
			case "of":
				tokens = append(tokens, token{kind: tokenOf})
			case "multiplied", "divided":
				// The following word must be "by"; skip it here and map the
				// pair to the operator.
				j := i
				for j < len(runes) && unicode.IsSpace(runes[j]) {
					j++
				}
				startBy := j
				for j < len(runes) && unicode.IsLetter(runes[j]) {
					j++
				}
				if strings.ToLower(string(runes[startBy:j])) != "by" {
					return nil, errors.New("I couldn't read that as arithmetic.")
				}
				if word == "multiplied" {
					tokens = append(tokens, token{kind: tokenStar})
				} else {
					tokens = append(tokens, token{kind: tokenSlash})
				}
				i = j
			default:
				return nil, errors.New("I couldn't read that as arithmetic.")
			}
		default:
			var kind tokenKind
			switch r {
			case '+':
				kind = tokenPlus
			case '-', '−', '–':
				kind = tokenMinus
			case '*', '×':
				kind = tokenStar
			case '/', '÷':
				kind = tokenSlash
			case '%':
				kind = tokenPercent
			case '(':
				kind = tokenLParen
			case ')':
				kind = tokenRParen
			default:
				return nil, errors.New("I couldn't read that as arithmetic.")
			}
			tokens = append(tokens, token{kind: kind})
			i++
		}
		if len(tokens) > maxTokens {
			return nil, errors.New("That calculation has too many steps.")
		}
	}
	return tokens, nil
}

// parser is a recursive-descent parser over the whitelisted tokens. Percent is
// a postfix operator: 15% of 80 is (15 / 100) * 80.
type parser struct {
	tokens []token
	pos    int
	depth  int
	ops    int
}

func (p *parser) parseExpression() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for p.peek(tokenPlus) || p.peek(tokenMinus) {
		kind := p.tokens[p.pos].kind
		p.pos++
		if err := p.step(); err != nil {
			return 0, err
		}
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if kind == tokenPlus {
			left += right
		} else {
			left -= right
		}
		if err := checkResult(left); err != nil {
			return 0, err
		}
	}
	return left, nil
}

func (p *parser) parseTerm() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for p.peek(tokenStar) || p.peek(tokenSlash) || p.peek(tokenOf) {
		kind := p.tokens[p.pos].kind
		p.pos++
		if err := p.step(); err != nil {
			return 0, err
		}
		right, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		switch kind {
		case tokenStar, tokenOf:
			left *= right
		case tokenSlash:
			if right == 0 {
				return 0, errors.New("I can't divide by zero.")
			}
			left /= right
		}
		if err := checkResult(left); err != nil {
			return 0, err
		}
	}
	return left, nil
}

func (p *parser) parseUnary() (float64, error) {
	if p.peek(tokenPlus) || p.peek(tokenMinus) {
		kind := p.tokens[p.pos].kind
		p.pos++
		if err := p.step(); err != nil {
			return 0, err
		}
		value, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if kind == tokenMinus {
			value = -value
		}
		return value, checkResult(value)
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (float64, error) {
	value, err := p.parsePrimary()
	if err != nil {
		return 0, err
	}
	for p.peek(tokenPercent) {
		p.pos++
		if err := p.step(); err != nil {
			return 0, err
		}
		value /= 100
		if err := checkResult(value); err != nil {
			return 0, err
		}
	}
	return value, nil
}

func (p *parser) parsePrimary() (float64, error) {
	if p.pos >= len(p.tokens) {
		return 0, errors.New("I couldn't read that as arithmetic.")
	}
	current := p.tokens[p.pos]
	if current.kind == tokenNumber {
		p.pos++
		return current.value, nil
	}
	if current.kind != tokenLParen {
		return 0, errors.New("I couldn't read that as arithmetic.")
	}
	p.pos++
	p.depth++
	if p.depth > maxDepth {
		return 0, errors.New("That calculation has too many nested parentheses.")
	}
	value, err := p.parseExpression()
	if err != nil {
		return 0, err
	}
	if !p.peek(tokenRParen) {
		return 0, errors.New("I couldn't read that as arithmetic.")
	}
	p.pos++
	p.depth--
	return value, nil
}

// peek reports whether the next token has kind.
func (p *parser) peek(kind tokenKind) bool {
	return p.pos < len(p.tokens) && p.tokens[p.pos].kind == kind
}

// step counts one operation and enforces the work bound.
func (p *parser) step() error {
	p.ops++
	if p.ops > maxOperations {
		return errors.New("That calculation has too many steps.")
	}
	return nil
}

// checkResult rejects a non-finite or unreadably large intermediate value.
func checkResult(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("That result is too large.")
	}
	if math.Abs(value) > maxResult {
		return errors.New("That result is too large.")
	}
	return nil
}

// formatNumber trims a float to a short form suitable for speech. Six decimal
// places is enough for everyday arithmetic; a tiny non-zero value falls back to
// significant digits so it does not speak as zero.
func formatNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	if math.Abs(value) < 0.000001 {
		return strconv.FormatFloat(value, 'g', 6, 64)
	}
	text := strconv.FormatFloat(value, 'f', 6, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "-0" {
		return "0"
	}
	return text
}

func containsDigit(text string) bool {
	for _, r := range text {
		if isDigit(r) {
			return true
		}
	}
	return false
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// Module is the calc Tool domain. It has no settings.
type Module struct{}

// NewModule builds the calc module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "calc" }

// Register adds calc.evaluate.
func (*Module) Register(r *tool.Registry, _ tool.Deps) error {
	return tool.Add(r, New())
}
