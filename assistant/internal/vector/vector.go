// Package vector holds the small float32 helpers shared by embedding-based
// matching. Every caller compares unit-length vectors, so Dot is the cosine
// similarity.
package vector

import "math"

// Normalize returns vec scaled to unit length. A zero vector is returned as a
// copy rather than divided by zero; Dot against it is zero.
func Normalize(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return append([]float32{}, vec...)
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = float32(float64(v) / norm)
	}
	return out
}

// Dot returns the inner product of a and b. For unit vectors this is the
// cosine similarity.
func Dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// Cosine returns the cosine similarity of a and b. It normalizes both sides,
// so callers can pass the raw embed service vectors.
func Cosine(a, b []float32) float64 {
	return Dot(Normalize(a), Normalize(b))
}
