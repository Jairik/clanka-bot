package units_test

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/units"
)

func newRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(2 * time.Second)
	if err := units.NewModule().Register(registry, tool.Deps{}); err != nil {
		t.Fatalf("register units: %v", err)
	}
	return registry
}

func convert(t *testing.T, value float64, from, to string) (units.Out, error) {
	t.Helper()
	args, err := json.Marshal(units.In{Value: value, From: from, To: to})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := newRegistry(t).Execute(context.Background(), units.Name, args)
	if err != nil {
		return units.Out{}, err
	}
	out, ok := result.(units.Out)
	if !ok {
		t.Fatalf("result type = %T, want units.Out", result)
	}
	return out, nil
}

// TestKnownConversions uses independent literal expectations, not a second
// implementation of the conversion table.
func TestKnownConversions(t *testing.T) {
	tests := []struct {
		value    float64
		from, to string
		want     float64
	}{
		{5, "miles", "km", 8.04672},
		{1, "inch", "cm", 2.54},
		{1, "foot", "inches", 12},
		{1, "mile", "feet", 5280},
		{1, "km", "miles", 0.621371192237334},
		{100, "celsius", "fahrenheit", 212},
		{32, "fahrenheit", "celsius", 0},
		{0, "celsius", "kelvin", 273.15},
		{273.15, "kelvin", "celsius", 0},
		{98.6, "fahrenheit", "celsius", 37},
		{-40, "c", "f", -40},
		{1, "kg", "lb", 2.2046226218487757},
		{1, "lb", "g", 453.59237},
		{1, "gallon", "liters", 3.785411784},
		{1, "cup", "ml", 236.5882365},
		{1, "day", "minutes", 1440},
		{1, "hour", "seconds", 3600},
		{60, "mph", "kph", 96.56064},
		{1, "acre", "square meters", 4046.8564224},
		{1, "square kilometer", "hectares", 100},
		{1, "nautical mile", "m", 1852},
	}
	for _, test := range tests {
		name := strings.Join([]string{formatTest(test.value), test.from, "to", test.to}, " ")
		t.Run(name, func(t *testing.T) {
			out, err := convert(t, test.value, test.from, test.to)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if math.Abs(out.Result-test.want) > 1e-6 {
				t.Fatalf("result = %.12f, want %.12f", out.Result, test.want)
			}
			if out.From == "" || out.To == "" || out.Display == "" {
				t.Fatalf("out = %+v, want metadata and display", out)
			}
		})
	}
}

// formatTest keeps subtest names readable without importing the package's
// private formatter.
func formatTest(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// TestDimensionMismatchNamesBothUnits checks the helpful error path.
func TestDimensionMismatchNamesBothUnits(t *testing.T) {
	_, err := convert(t, 5, "miles", "kilograms")
	if err == nil {
		t.Fatal("convert miles to kilograms returned no error")
	}
	message := tool.FailureMessage(err)
	if !strings.Contains(message, "miles") || !strings.Contains(message, "kilograms") {
		t.Fatalf("error = %q, want both units named", message)
	}
}

// TestUnknownUnitsAreRejected keeps currency and invented units out without any
// dynamic lookup.
func TestUnknownUnitsAreRejected(t *testing.T) {
	for _, test := range []struct {
		from, to string
	}{
		{"dollars", "euros"},
		{"miles", "euros"},
		{"parsecs", "km"},
	} {
		if _, err := convert(t, 5, test.from, test.to); err == nil {
			t.Fatalf("convert %s to %s returned no error", test.from, test.to)
		}
	}
}

// TestNonFiniteAndOverflowRejected covers the numeric bounds. Inf and NaN
// cannot cross JSON, so those cases use the public handler field directly.
func TestNonFiniteAndOverflowRejected(t *testing.T) {
	handler := units.New().Handler
	if _, err := handler(context.Background(), units.In{Value: math.Inf(1), From: "miles", To: "km"}); err == nil {
		t.Fatal("infinite value returned no error")
	}
	if _, err := handler(context.Background(), units.In{Value: math.NaN(), From: "miles", To: "km"}); err == nil {
		t.Fatal("NaN value returned no error")
	}
	if _, err := convert(t, 1e15, "miles", "km"); err == nil {
		t.Fatal("overflowing conversion returned no error")
	}
}

// TestSpeakUsesLongUnitNames checks the spoken form, not just the number.
func TestSpeakUsesLongUnitNames(t *testing.T) {
	out, err := convert(t, 5, "mi", "km")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	text, ok := newRegistry(t).Speak(units.Name, out)
	if !ok {
		t.Fatal("Speak returned no text")
	}
	want := "5 miles is 8.04672 kilometers."
	if text != want {
		t.Fatalf("Speak = %q, want %q", text, want)
	}
}

// TestExtractPhrasings covers the natural word orders users speak.
func TestExtractPhrasings(t *testing.T) {
	registry := newRegistry(t)
	tests := []struct {
		utterance string
		value     float64
		from      string
		to        string
	}{
		{"convert 5 miles to km", 5, "miles", "km"},
		{"what is 5 miles in kilometers", 5, "miles", "kilometers"},
		{"how many kilometers in 5 miles", 5, "miles", "kilometers"},
		{"convert 100 fahrenheit to celsius", 100, "fahrenheit", "celsius"},
		{"how many minutes in a day", 1, "day", "minutes"},
		{"convert 1 kilogram to pounds", 1, "kilogram", "pounds"},
		{"what is 30 celsius in fahrenheit", 30, "celsius", "fahrenheit"},
		{"convert 2 gallons to liters", 2, "gallons", "liters"},
		{"how many feet in a mile", 1, "mile", "feet"},
		{"5 in in cm", 5, "in", "cm"},
		{"convert 5 in to cm", 5, "in", "cm"},
		{"how many cm in 5 in", 5, "in", "cm"},
		{"5 miles into kilometers", 5, "miles", "kilometers"},
	}
	for _, test := range tests {
		t.Run(test.utterance, func(t *testing.T) {
			args, ok := registry.Extract(units.Name, test.utterance)
			if !ok {
				t.Fatalf("Extract(%q) failed", test.utterance)
			}
			var in units.In
			if err := json.Unmarshal(args, &in); err != nil {
				t.Fatalf("decode args: %v", err)
			}
			if in.Value != test.value || in.From != test.from || in.To != test.to {
				t.Fatalf("Extract(%q) = %+v, want %v %s to %s", test.utterance, in, test.value, test.from, test.to)
			}
		})
	}
}

// TestExtractKeepsUnsupportedQuestionsOnTheAgentPath checks that unknown units
// and non-conversion questions do not become Fast path guesses.
func TestExtractKeepsUnsupportedQuestionsOnTheAgentPath(t *testing.T) {
	registry := newRegistry(t)
	for _, utterance := range []string{
		"how many calories in an apple",
		"what's the exchange rate for euros",
		"convert my garage into a bedroom",
		"how many people live in Tokyo",
		"what's the time difference between here and Tokyo",
		"how tall is Mount Everest",
	} {
		if _, ok := registry.Extract(units.Name, utterance); ok {
			t.Fatalf("Extract(%q) succeeded, want the Agent path", utterance)
		}
	}
}
