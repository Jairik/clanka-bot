package namematch

import (
	"fmt"
	"testing"
)

// cacheFill builds count distinct texts and a fixed vector for each.
func cacheFill(count int) ([]string, [][]float32) {
	texts := make([]string, count)
	vectors := make([][]float32, count)
	for i := range texts {
		texts[i] = fmt.Sprintf("name-%d", i)
		vectors[i] = []float32{1, 0}
	}
	return texts, vectors
}

func TestCacheIsBoundedAndClearsWhenFull(t *testing.T) {
	matcher := New(nil, Config{})

	texts, vectors := cacheFill(maxCachedVectors - 1)
	matcher.storeCached(texts, vectors, 2)
	if got := len(matcher.cached); got != maxCachedVectors-1 {
		t.Fatalf("cached entries = %d, want %d", got, maxCachedVectors-1)
	}
	if matcher.dims != 2 {
		t.Fatalf("dims = %d, want 2", matcher.dims)
	}

	// Two more entries would overflow the bound, so the cache is cleared and
	// refilled with only this batch rather than growing without limit.
	extra := []string{"extra-a", "extra-b"}
	matcher.storeCached(extra, [][]float32{{1, 0}, {1, 0}}, 2)
	if got := len(matcher.cached); got != len(extra) {
		t.Fatalf("cached entries after overflow = %d, want %d", got, len(extra))
	}
	for _, text := range extra {
		if _, ok := matcher.cached[text]; !ok {
			t.Fatalf("cached entries after overflow = %v, want %q present", matcher.cached, text)
		}
	}
}

func TestCacheNeverExceedsTheBoundForOneHugeBatch(t *testing.T) {
	matcher := New(nil, Config{})

	texts, vectors := cacheFill(maxCachedVectors + 500)
	matcher.storeCached(texts, vectors, 2)
	if got := len(matcher.cached); got != maxCachedVectors {
		t.Fatalf("cached entries = %d, want the bound %d", got, maxCachedVectors)
	}
}

func TestCacheClearsWhenTheModelDimensionChanges(t *testing.T) {
	matcher := New(nil, Config{})

	matcher.storeCached([]string{"birds", "rain"}, [][]float32{{1, 0}, {0, 1}}, 2)
	if got := len(matcher.cached); got != 2 {
		t.Fatalf("cached entries = %d, want 2", got)
	}

	matcher.storeCached([]string{"birds"}, [][]float32{{1, 0, 0}}, 3)
	if got := len(matcher.cached); got != 1 {
		t.Fatalf("cached entries after a dimension change = %d, want only the new batch", got)
	}
	if vec, ok := matcher.cached["birds"]; !ok || len(vec) != 3 {
		t.Fatalf("cached birds = %v, want a three-dimension vector", vec)
	}
	if _, ok := matcher.cached["rain"]; ok {
		t.Fatal("a vector from the old model must not survive a dimension change")
	}
	if matcher.dims != 3 {
		t.Fatalf("dims = %d, want 3", matcher.dims)
	}
}

func TestStoreCachedIgnoresAnInconsistentBatch(t *testing.T) {
	matcher := New(nil, Config{})
	matcher.storeCached([]string{"birds"}, [][]float32{{1, 0}, {0, 1}}, 2)
	if got := len(matcher.cached); got != 0 {
		t.Fatalf("cached entries = %d, want 0 for a batch whose vectors do not match its texts", got)
	}
}
