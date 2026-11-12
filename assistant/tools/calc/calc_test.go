package calc_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/calc"
)

// newRegistry registers the real Tool so every test crosses the public seam.
func newRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(2 * time.Second)
	if err := calc.NewModule().Register(registry, tool.Deps{}); err != nil {
		t.Fatalf("register calc: %v", err)
	}
	return registry
}

// evaluate runs one expression through registry.Execute.
func evaluate(t *testing.T, expression string) (calc.Out, error) {
	t.Helper()
	args, err := json.Marshal(calc.In{Expression: expression})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := newRegistry(t).Execute(context.Background(), calc.Name, args)
	if err != nil {
		return calc.Out{}, err
	}
	out, ok := result.(calc.Out)
	if !ok {
		t.Fatalf("result type = %T, want calc.Out", result)
	}
	return out, nil
}

// TestEvaluateKnownArithmetic uses independent literal expectations, so a
// change in parser precedence or percentage handling cannot move the answer
// silently.
func TestEvaluateKnownArithmetic(t *testing.T) {
	tests := []struct {
		expression string
		want       float64
		display    string
	}{
		{"15% of 80", 12, "12"},
		{"15%of80", 12, "12"},
		{"15 % of 80", 12, "12"},
		{"15 percent of 80", 12, "12"},
		{"20% of 150", 30, "30"},
		{"50% of 80", 40, "40"},
		{"2+2", 4, "4"},
		{"17 times 24", 408, "408"},
		{"100 divided by 4", 25, "25"},
		{"3 plus 4 times 2", 11, "11"},
		{"(3 plus 4) times 2", 14, "14"},
		{"10 minus 3", 7, "7"},
		{"5 - 8", -3, "-3"},
		{"1,000 + 2", 1002, "1002"},
		{"8 times 9", 72, "72"},
		{"3 x 4", 12, "12"},
		{"1 over 4", 0.25, "0.25"},
		{"2 multiplied by 6", 12, "12"},
		{"100 - 15%", 99.85, "99.85"},
		{"5 / 2", 2.5, "2.5"},
		{".5 + .25", 0.75, "0.75"},
		{"7", 7, "7"},
		{"2 × 3", 6, "6"},
		{"9 ÷ 3", 3, "3"},
	}
	for _, test := range tests {
		t.Run(test.expression, func(t *testing.T) {
			out, err := evaluate(t, test.expression)
			if err != nil {
				t.Fatalf("Evaluate(%q): %v", test.expression, err)
			}
			if math.Abs(out.Value-test.want) > 1e-9 {
				t.Fatalf("Evaluate(%q) = %v, want %v", test.expression, out.Value, test.want)
			}
			if out.Display != test.display {
				t.Fatalf("display(%q) = %q, want %q", test.expression, out.Display, test.display)
			}
		})
	}
}

// TestPercentageSpeaksTheRequest checks the public Speak renderer.
func TestPercentageSpeaksTheRequest(t *testing.T) {
	out, err := evaluate(t, "15% of 80")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	text, ok := newRegistry(t).Speak(calc.Name, out)
	if !ok {
		t.Fatal("Speak returned no text")
	}
	if text != "15% of 80 is 12." {
		t.Fatalf("Speak = %q, want %q", text, "15% of 80 is 12.")
	}
}

// TestMalformedAndUnsafeInputFailsHelpfully checks that the whitelist rejects
// code-like input and that arithmetic failures say what went wrong.
func TestMalformedAndUnsafeInputFailsHelpfully(t *testing.T) {
	tests := []struct {
		expression string
		want       string
	}{
		{"", "Tell me the calculation"},
		{"1 +", "couldn't read"},
		{"1..2", "couldn't read"},
		{"sqrt(4)", "couldn't read"},
		{"two plus two", "couldn't read"},
		{"1; rm -rf /", "couldn't read"},
		{"1 + __import__('os')", "couldn't read"},
		{"1 / 0", "divide by zero"},
		{"10 / (5 - 5)", "divide by zero"},
		{"9999999999999999", "too large"},
		{"999999999999999 * 999999999999999", "too large"},
	}
	for _, test := range tests {
		t.Run(test.expression, func(t *testing.T) {
			_, err := evaluate(t, test.expression)
			if err == nil {
				t.Fatalf("Evaluate(%q) returned no error", test.expression)
			}
			message := tool.FailureMessage(err)
			if !strings.Contains(strings.ToLower(message), strings.ToLower(test.want)) {
				t.Fatalf("error = %q, want it to contain %q", message, test.want)
			}
		})
	}
}

// TestBoundsRejectLongDeepAndBusyInput keeps one request from doing unbounded
// work even when every token is legal.
func TestBoundsRejectLongDeepAndBusyInput(t *testing.T) {
	deep := strings.Repeat("(", 20) + "1" + strings.Repeat(")", 20)
	unary := "1" + strings.Repeat("-", 70) + "1"
	long := strings.Repeat("1+", 150) + "1"
	tests := []struct {
		name       string
		expression string
		want       string
	}{
		{"too long", long, "too long"},
		{"too deep", deep, "nested"},
		{"too many steps", unary, "too many steps"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := evaluate(t, test.expression)
			if err == nil {
				t.Fatalf("Evaluate(%q) returned no error", test.expression)
			}
			if message := tool.FailureMessage(err); !strings.Contains(strings.ToLower(message), test.want) {
				t.Fatalf("error = %q, want it to contain %q", message, test.want)
			}
		})
	}
}

// TestExtractKeepsUnsupportedQuestionsOnTheAgentPath checks the Fast path
// boundary instead of only the handler.
func TestExtractKeepsUnsupportedQuestionsOnTheAgentPath(t *testing.T) {
	registry := newRegistry(t)
	tests := []struct {
		utterance string
		want      string
	}{
		{"what is 15% of 80", "15% of 80"},
		{"what is 15%of80", "15%of80"},
		{"what's 17 times 24", "17 times 24"},
		{"calculate 12 plus 7", "12 plus 7"},
		{"how much is 100 divided by 4", "100 divided by 4"},
		{"what's 20 percent of 150", "20 percent of 150"},
		{"please compute 3 plus 4 times 2", "3 plus 4 times 2"},
	}
	for _, test := range tests {
		t.Run(test.utterance, func(t *testing.T) {
			args, ok := registry.Extract(calc.Name, test.utterance)
			if !ok {
				t.Fatalf("Extract(%q) failed", test.utterance)
			}
			var in calc.In
			if err := json.Unmarshal(args, &in); err != nil {
				t.Fatalf("decode args: %v", err)
			}
			if in.Expression != test.want {
				t.Fatalf("Extract(%q) = %q, want %q", test.utterance, in.Expression, test.want)
			}
		})
	}
	for _, utterance := range []string{
		"what's the square root of 144",
		"what is the capital of France",
		"calculate my taxes",
		"translate 12 into Spanish",
		"what is 1 divided by zero",
	} {
		if _, ok := registry.Extract(calc.Name, utterance); ok {
			t.Fatalf("Extract(%q) succeeded, want the Agent path", utterance)
		}
	}
}
