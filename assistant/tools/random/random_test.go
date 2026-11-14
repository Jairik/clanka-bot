package random_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/random"
)

// sequence is the injected randomness seam: it returns the planned values and
// records the bounds the Tools asked for.
type sequence struct {
	values []int
	bounds []int
	at     int
}

func (s *sequence) Intn(n int) (int, error) {
	s.bounds = append(s.bounds, n)
	if s.at >= len(s.values) {
		return 0, errors.New("sequence exhausted")
	}
	value := s.values[s.at]
	s.at++
	if value < 0 || value >= n {
		return 0, fmt.Errorf("planned value %d is outside [0,%d)", value, n)
	}
	return value, nil
}

func moduleWith(values ...int) (*random.Module, *sequence) {
	source := &sequence{values: values}
	return random.NewModuleWithSource(source.Intn), source
}

func registryFor(t *testing.T, module *random.Module) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(2 * time.Second)
	if err := module.Register(registry, tool.Deps{}); err != nil {
		t.Fatalf("register random: %v", err)
	}
	return registry
}

func execute[T any](t *testing.T, registry *tool.Registry, name string, args any) (T, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := registry.Execute(context.Background(), name, raw)
	if err != nil {
		var zero T
		return zero, err
	}
	out, ok := result.(T)
	if !ok {
		t.Fatalf("result type = %T, want %T", result, *new(T))
	}
	return out, nil
}

// TestFlipUsesBothSides checks the seam mapping and the spoken words.
func TestFlipUsesBothSides(t *testing.T) {
	module, source := moduleWith(0)
	registry := registryFor(t, module)
	heads, err := execute[random.FlipOut](t, registry, random.FlipName, random.FlipIn{})
	if err != nil {
		t.Fatalf("flip: %v", err)
	}
	if heads.Side != "heads" {
		t.Fatalf("Side = %q, want heads", heads.Side)
	}
	if source.bounds[0] != 2 {
		t.Fatalf("flip bound = %d, want 2", source.bounds[0])
	}
	if text, _ := registry.Speak(random.FlipName, heads); text != "Heads." {
		t.Fatalf("Speak = %q, want Heads.", text)
	}

	module, _ = moduleWith(1)
	registry = registryFor(t, module)
	tails, err := execute[random.FlipOut](t, registry, random.FlipName, random.FlipIn{})
	if err != nil {
		t.Fatalf("flip: %v", err)
	}
	if tails.Side != "tails" {
		t.Fatalf("Side = %q, want tails", tails.Side)
	}
	if text, _ := registry.Speak(random.FlipName, tails); text != "Tails." {
		t.Fatalf("Speak = %q, want Tails.", text)
	}
}

// TestD20IsInclusive checks both ends of the d20 range: mapped zero is 1 and
// mapped nineteen is 20.
func TestD20IsInclusive(t *testing.T) {
	for _, test := range []struct {
		rolled int
		want   int
	}{{0, 1}, {19, 20}} {
		module, source := moduleWith(test.rolled)
		registry := registryFor(t, module)
		out, err := execute[random.RollOut](t, registry, random.RollName, random.RollIn{Count: 1, Sides: 20})
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		if out.Total != test.want || len(out.Rolls) != 1 || out.Rolls[0] != test.want {
			t.Fatalf("roll = %+v, want %d", out, test.want)
		}
		if source.bounds[0] != 20 {
			t.Fatalf("roll bound = %d, want 20", source.bounds[0])
		}
	}
}

// TestMultipleDiceKeepEachResult checks the rolls and the total.
func TestMultipleDiceKeepEachResult(t *testing.T) {
	module, source := moduleWith(2, 4)
	registry := registryFor(t, module)
	out, err := execute[random.RollOut](t, registry, random.RollName, random.RollIn{Count: 2, Sides: 6})
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if out.Total != 8 || len(out.Rolls) != 2 || out.Rolls[0] != 3 || out.Rolls[1] != 5 {
		t.Fatalf("roll = %+v, want rolls [3 5] total 8", out)
	}
	if len(source.bounds) != 2 || source.bounds[0] != 6 || source.bounds[1] != 6 {
		t.Fatalf("bounds = %v, want [6 6]", source.bounds)
	}
	if text, _ := registry.Speak(random.RollName, out); text != "You rolled 2 dice for a total of 8." {
		t.Fatalf("Speak = %q", text)
	}
}

// TestRollDefaultsToOneD6 checks the agent-facing zero values.
func TestRollDefaultsToOneD6(t *testing.T) {
	module, source := moduleWith(5)
	registry := registryFor(t, module)
	out, err := execute[random.RollOut](t, registry, random.RollName, random.RollIn{})
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if out.Count != 1 || out.Sides != 6 || out.Total != 6 {
		t.Fatalf("roll = %+v, want one d6", out)
	}
	if source.bounds[0] != 6 {
		t.Fatalf("bound = %d, want 6", source.bounds[0])
	}
	if text, _ := registry.Speak(random.RollName, out); text != "You rolled 6." {
		t.Fatalf("Speak = %q", text)
	}
}

// TestRollBoundsRefuseHugeRequests checks the configured caps.
func TestRollBoundsRefuseHugeRequests(t *testing.T) {
	module, source := moduleWith()
	registry := registryFor(t, module)
	for _, in := range []random.RollIn{
		{Count: 101, Sides: 6},
		{Count: 1, Sides: 1},
		{Count: 1, Sides: 1001},
	} {
		if _, err := execute[random.RollOut](t, registry, random.RollName, in); err == nil {
			t.Fatalf("roll %+v returned no error", in)
		}
	}
	if len(source.bounds) != 0 {
		t.Fatalf("invalid bounds drew %v, want no draws", source.bounds)
	}
}

// TestPickPreservesTheOptions checks exact words, not semantic labels.
func TestPickPreservesTheOptions(t *testing.T) {
	module, source := moduleWith(1)
	registry := registryFor(t, module)
	options := []string{"the red one", "the blue one"}
	out, err := execute[random.PickOut](t, registry, random.PickName, random.PickIn{Options: options})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if out.Choice != "the blue one" || out.Index != 1 {
		t.Fatalf("pick = %+v, want the blue one at 1", out)
	}
	if len(out.Options) != 2 || out.Options[0] != "the red one" || out.Options[1] != "the blue one" {
		t.Fatalf("options = %#v, want the original words", out.Options)
	}
	if source.bounds[0] != 2 {
		t.Fatalf("bound = %d, want 2", source.bounds[0])
	}
	if text, _ := registry.Speak(random.PickName, out); text != "I'd pick the blue one." {
		t.Fatalf("Speak = %q", text)
	}
}

// TestPickLimits checks the empty, huge and overlong option errors.
func TestPickLimits(t *testing.T) {
	module, _ := moduleWith(0)
	registry := registryFor(t, module)
	if _, err := execute[random.PickOut](t, registry, random.PickName, random.PickIn{}); err == nil {
		t.Fatal("empty options returned no error")
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("option %d", i)
	}
	if _, err := execute[random.PickOut](t, registry, random.PickName, random.PickIn{Options: many}); err == nil {
		t.Fatal("51 options returned no error")
	}
	long := strings.Repeat("x", 101)
	if _, err := execute[random.PickOut](t, registry, random.PickName, random.PickIn{Options: []string{long, "other"}}); err == nil {
		t.Fatal("overlong option returned no error")
	}
}

// TestExtractRollPhrasings covers d-notation, sided dice and count forms.
func TestExtractRollPhrasings(t *testing.T) {
	registry := registryFor(t, random.NewModule())
	tests := []struct {
		utterance string
		want      random.RollIn
	}{
		{"roll a d20", random.RollIn{Count: 1, Sides: 20}},
		{"roll d20", random.RollIn{Count: 1, Sides: 20}},
		{"roll 2d6", random.RollIn{Count: 2, Sides: 6}},
		{"roll two d6", random.RollIn{Count: 2, Sides: 6}},
		{"roll a six sided die", random.RollIn{Count: 1, Sides: 6}},
		{"roll a twenty sided die", random.RollIn{Count: 1, Sides: 20}},
		{"roll 2 six-sided dice", random.RollIn{Count: 2, Sides: 6}},
		{"roll 3 dice", random.RollIn{Count: 3, Sides: 6}},
		{"roll two dice with eight sides", random.RollIn{Count: 2, Sides: 8}},
		{"roll a die", random.RollIn{Count: 1, Sides: 6}},
		{"roll a pair of dice", random.RollIn{Count: 2, Sides: 6}},
		{"roll 1001 d20", random.RollIn{Count: 1001, Sides: 20}},
	}
	for _, test := range tests {
		t.Run(test.utterance, func(t *testing.T) {
			args, ok := registry.Extract(random.RollName, test.utterance)
			if !ok {
				t.Fatalf("Extract(%q) failed", test.utterance)
			}
			var in random.RollIn
			if err := json.Unmarshal(args, &in); err != nil {
				t.Fatalf("decode args: %v", err)
			}
			if in != test.want {
				t.Fatalf("Extract(%q) = %+v, want %+v", test.utterance, in, test.want)
			}
		})
	}
}

// TestExtractPickPhrasings covers between, from and comma lists.
func TestExtractPickPhrasings(t *testing.T) {
	registry := registryFor(t, random.NewModule())
	tests := []struct {
		utterance string
		want      []string
	}{
		{"pick between pizza and tacos", []string{"pizza", "tacos"}},
		{"choose between tea and coffee", []string{"tea", "coffee"}},
		{"pick from pizza, tacos, or sushi", []string{"pizza", "tacos", "sushi"}},
		{"pick one: red or blue", []string{"red", "blue"}},
		{"pick between the red one and the blue one", []string{"the red one", "the blue one"}},
		{"randomly pick between the red one and the blue one", []string{"the red one", "the blue one"}},
		{"help me choose between a cat and a dog", []string{"a cat", "a dog"}},
		{"pick pizza or tacos", []string{"pizza", "tacos"}},
	}
	for _, test := range tests {
		t.Run(test.utterance, func(t *testing.T) {
			args, ok := registry.Extract(random.PickName, test.utterance)
			if !ok {
				t.Fatalf("Extract(%q) failed", test.utterance)
			}
			var in random.PickIn
			if err := json.Unmarshal(args, &in); err != nil {
				t.Fatalf("decode args: %v", err)
			}
			if len(in.Options) != len(test.want) {
				t.Fatalf("Extract(%q) = %#v, want %#v", test.utterance, in.Options, test.want)
			}
			for i := range test.want {
				if in.Options[i] != test.want[i] {
					t.Fatalf("Extract(%q) = %#v, want %#v", test.utterance, in.Options, test.want)
				}
			}
		})
	}
}

// TestExtractKeepsNonDiceAndNonPickRequestsOnTheAgentPath checks the Fast path
// boundary for the two argument Tools.
func TestExtractKeepsNonDiceAndNonPickRequestsOnTheAgentPath(t *testing.T) {
	registry := registryFor(t, random.NewModule())
	for _, test := range []struct {
		name      string
		utterance string
	}{
		{random.RollName, "roll out the red carpet"},
		{random.RollName, "roll up the window"},
		{random.PickName, "pick a restaurant"},
		{random.PickName, "pick a number between 1 and 10"},
		{random.PickName, "pick up the groceries"},
		{random.PickName, "what is the capital of France"},
	} {
		if _, ok := registry.Extract(test.name, test.utterance); ok {
			t.Fatalf("Extract(%s, %q) succeeded, want the Agent path", test.name, test.utterance)
		}
	}
}

// TestCryptoSourceStaysInRange exercises the production seam over many rolls;
// it asserts the range only, never a particular random outcome.
func TestCryptoSourceStaysInRange(t *testing.T) {
	registry := registryFor(t, random.NewModule())
	for i := 0; i < 200; i++ {
		out, err := execute[random.RollOut](t, registry, random.RollName, random.RollIn{Count: 1, Sides: 20})
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		if out.Total < 1 || out.Total > 20 {
			t.Fatalf("d20 = %d, want 1..20", out.Total)
		}
	}
}
