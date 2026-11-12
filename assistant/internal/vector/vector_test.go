package vector_test

import (
	"math"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/vector"
)

func TestCosineAndNormalize(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"parallel", []float32{2, 0}, []float32{5, 0}, 1},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"already unit", []float32{0.8, 0.6}, []float32{1, 0}, 0.8},
		{"zero vector", []float32{0, 0}, []float32{1, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vector.Cosine(tc.a, tc.b); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("Cosine = %.6f, want %.6f", got, tc.want)
			}
		})
	}

	// Normalize must not mutate the caller's slice and must return a copy of a
	// zero vector rather than dividing by zero.
	input := []float32{3, 4}
	unit := vector.Normalize(input)
	if input[0] != 3 || input[1] != 4 {
		t.Fatalf("Normalize mutated its input: %v", input)
	}
	if math.Abs(float64(unit[0])-0.6) > 1e-6 || math.Abs(float64(unit[1])-0.8) > 1e-6 {
		t.Fatalf("Normalize(3,4) = %v, want (0.6, 0.8)", unit)
	}
	zero := vector.Normalize([]float32{0, 0})
	if zero[0] != 0 || zero[1] != 0 {
		t.Fatalf("Normalize(zero) = %v, want a zero copy", zero)
	}
}
