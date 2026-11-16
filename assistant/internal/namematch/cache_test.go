package namematch

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// cacheEmbedder answers every text with a deterministic vector and records each
// batch, so a test can count what the cache failed to serve. It is safe for
// concurrent use because the race test shares one matcher.
type cacheEmbedder struct {
	mu      sync.Mutex
	batches [][]string
}

func (e *cacheEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.batches = append(e.batches, append([]string(nil), texts...))
	e.mu.Unlock()
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		// Any finite, non-zero vector will do: these tests care about which
		// texts reach the embedder, not about the score.
		vectors[i] = []float32{1, float32(len(text) % 7)}
	}
	return vectors, nil
}

func (e *cacheEmbedder) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.batches)
}

func (e *cacheEmbedder) last() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.batches) == 0 {
		return nil
	}
	return append([]string(nil), e.batches[len(e.batches)-1]...)
}

// embeddedSince totals the texts in the batches from index from onward, so a
// test can count one batched warm without assuming the whole library arrived
// in a single request.
func (e *cacheEmbedder) embeddedSince(from int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := 0
	for _, batch := range e.batches[from:] {
		total += len(batch)
	}
	return total
}

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

func TestCacheStaysBoundedAndEvictsInsteadOfClearing(t *testing.T) {
	cache := newCandidateCache()
	texts, vectors := cacheFill(maxCachedVectors)
	cache.store(SourceSounds, texts, vectors, 2)
	if cache.total != maxCachedVectors {
		t.Fatalf("cached entries = %d, want the bound %d", cache.total, maxCachedVectors)
	}

	// One more entry evicts the least recently used one. The rest of the batch
	// survives, unlike the clear-on-overflow this cache replaced.
	cache.store(SourceSounds, []string{"extra"}, [][]float32{{1, 0}}, 2)
	if cache.total != maxCachedVectors {
		t.Fatalf("cached entries after overflow = %d, want the bound %d", cache.total, maxCachedVectors)
	}
	if _, ok := cache.get(SourceSounds, "extra"); !ok {
		t.Fatal("the newest entry must be cached")
	}
	if _, ok := cache.get(SourceSounds, "name-0"); ok {
		t.Fatal("the least recently used entry must have been evicted")
	}
	if _, ok := cache.get(SourceSounds, fmt.Sprintf("name-%d", maxCachedVectors-1)); !ok {
		t.Fatal("a recently stored entry must survive a single overflow")
	}
}

func TestCacheNeverExceedsTheBoundForOneHugeBatch(t *testing.T) {
	cache := newCandidateCache()

	texts, vectors := cacheFill(maxCachedVectors + 500)
	cache.store(SourceSounds, texts, vectors, 2)
	if cache.total != maxCachedVectors {
		t.Fatalf("cached entries = %d, want the bound %d", cache.total, maxCachedVectors)
	}
	// The batch is not cleared: the most recently stored part of it is kept.
	if _, ok := cache.get(SourceSounds, fmt.Sprintf("name-%d", maxCachedVectors+499)); !ok {
		t.Fatal("the last vector of a huge batch must be cached")
	}
	if _, ok := cache.get(SourceSounds, "name-0"); ok {
		t.Fatal("the first vector of a huge batch must have been evicted")
	}
}

// A warm of one source must not be discarded by a fill of another source that
// does not outgrow it.
func TestCacheFillsOfOneSourceDoNotEvictAnother(t *testing.T) {
	cache := newCandidateCache()
	cache.store(SourcePlaylists, []string{"warm playlist"}, [][]float32{{1, 0}}, 2)

	texts, vectors := cacheFill(maxCachedVectors + 500)
	cache.store(SourceSounds, texts, vectors, 2)

	if cache.total != maxCachedVectors {
		t.Fatalf("cached entries = %d, want the bound %d", cache.total, maxCachedVectors)
	}
	if _, ok := cache.get(SourcePlaylists, "warm playlist"); !ok {
		t.Fatal("a larger fill of another source must not evict a warmed source")
	}
}

// Two sources of equal size must not evict each other in turn: the source being
// filled gives way, so an idle library keeps its vectors.
func TestCacheEvictsTheSourceBeingFilledOnATie(t *testing.T) {
	cache := newCandidateCache()
	playlists, playlistVectors := cacheFill(maxCachedVectors / 2)
	sounds, soundVectors := cacheFill(maxCachedVectors / 2)
	cache.store(SourcePlaylists, playlists, playlistVectors, 2)
	cache.store(SourceSounds, sounds, soundVectors, 2)
	if cache.total != maxCachedVectors {
		t.Fatalf("cached entries = %d, want the bound %d", cache.total, maxCachedVectors)
	}

	cache.store(SourceSounds, []string{"extra"}, [][]float32{{1, 0}}, 2)
	if cache.total != maxCachedVectors {
		t.Fatalf("cached entries after overflow = %d, want the bound %d", cache.total, maxCachedVectors)
	}
	for _, text := range playlists {
		if _, ok := cache.get(SourcePlaylists, text); !ok {
			t.Fatalf("playlist %q was evicted by an equally sized source", text)
		}
	}
	if _, ok := cache.get(SourceSounds, "name-0"); ok {
		t.Fatal("the source being filled must give way first")
	}
}

func TestCacheKeepsTheSameTextPerSource(t *testing.T) {
	cache := newCandidateCache()

	cache.store(SourceSounds, []string{"rain"}, [][]float32{{1, 0}}, 2)
	cache.store(SourceListItems, []string{"rain"}, [][]float32{{0, 1}}, 2)

	if cache.total != 2 {
		t.Fatalf("cached entries = %d, want one per source", cache.total)
	}
	sounds, ok := cache.get(SourceSounds, "rain")
	if !ok || sounds[0] != 1 {
		t.Fatalf("sound rain = %v, want its own vector", sounds)
	}
	items, ok := cache.get(SourceListItems, "rain")
	if !ok || items[1] != 1 {
		t.Fatalf("list item rain = %v, want its own vector", items)
	}
}

func TestCacheResetsWhenTheModelDimensionChanges(t *testing.T) {
	cache := newCandidateCache()

	cache.store(SourceSounds, []string{"birds", "rain"}, [][]float32{{1, 0}, {0, 1}}, 2)
	cache.store(SourcePlaylists, []string{"mix"}, [][]float32{{1, 0}}, 2)
	if cache.total != 3 {
		t.Fatalf("cached entries = %d, want 3", cache.total)
	}

	// A redeployed model makes every stored vector unusable, whatever source
	// it belongs to.
	cache.store(SourceSounds, []string{"birds"}, [][]float32{{1, 0, 0}}, 3)
	if cache.total != 1 {
		t.Fatalf("cached entries after a dimension change = %d, want only the new batch", cache.total)
	}
	if vec, ok := cache.get(SourceSounds, "birds"); !ok || len(vec) != 3 {
		t.Fatalf("cached birds = %v, want a three-dimension vector", vec)
	}
	if _, ok := cache.get(SourceSounds, "rain"); ok {
		t.Fatal("a vector from the old model must not survive a dimension change")
	}
	if _, ok := cache.get(SourcePlaylists, "mix"); ok {
		t.Fatal("an old vector from another source must not survive a dimension change")
	}
	if cache.dims != 3 {
		t.Fatalf("dims = %d, want 3", cache.dims)
	}
}

func TestCacheIgnoresAnInconsistentBatch(t *testing.T) {
	cache := newCandidateCache()
	cache.store(SourceSounds, []string{"birds"}, [][]float32{{1, 0}, {0, 1}}, 2)
	if cache.total != 0 {
		t.Fatalf("cached entries = %d, want 0 for a batch whose vectors do not match its texts", cache.total)
	}
}

// TestWarmSurvivesAMixedWorkload is the reviewer's probe: warm one library,
// fill the cache from another source, then check that the warm's work is still
// there. Before per-source eviction the second source cleared the playlist
// vectors and the next playlist lookup embedded 1001 texts.
func TestWarmSurvivesAMixedWorkload(t *testing.T) {
	embedder := &cacheEmbedder{}
	matcher := New(embedder, Config{})
	ctx := context.Background()

	playlists := make([]string, 1000)
	for i := range playlists {
		playlists[i] = fmt.Sprintf("playlist name %d", i)
	}
	if err := matcher.Warm(ctx, SourcePlaylists, playlists); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if got := embedder.embeddedSince(0); got != len(playlists) {
		t.Fatalf("warm embedded %d texts, want %d", got, len(playlists))
	}

	// 1049 fuzzy list-item lookups, one item at a time, as a list resolves a
	// spoken reminder against one list.
	for i := 0; i < 1049; i++ {
		matcher.Match(ctx, SourceListItems, "fuzzy item", []string{fmt.Sprintf("list item %d", i)})
	}

	matcher.mu.Lock()
	kept := 0
	for _, name := range playlists {
		if _, ok := matcher.cache.get(SourcePlaylists, Normalize(name)); ok {
			kept++
		}
	}
	total := matcher.cache.total
	matcher.mu.Unlock()

	if kept != len(playlists) {
		t.Fatalf("playlist vectors cached after the fill = %d of %d, want all of them", kept, len(playlists))
	}
	if total > maxCachedVectors {
		t.Fatalf("cached entries = %d, want at most the bound %d", total, maxCachedVectors)
	}

	// The next playlist lookup must not embed a single candidate again.
	calls := embedder.calls()
	matcher.Match(ctx, SourcePlaylists, "the bird sound", playlists)
	if got := embedder.calls(); got != calls+1 {
		t.Fatalf("playlist lookup made %d embed calls, want 1", got-calls)
	}
	if got := embedder.last(); len(got) != 1 {
		t.Fatalf("playlist lookup embedded %d texts, want the query only", len(got))
	}
}

// TestLibraryLargerThanTheBudgetDoesNotOscillate proves a library above the
// bound reaches a steady state. The cache this replaced alternated between
// embedding the whole library and clearing to the few missing entries.
func TestLibraryLargerThanTheBudgetDoesNotOscillate(t *testing.T) {
	embedder := &cacheEmbedder{}
	matcher := New(embedder, Config{})
	ctx := context.Background()

	names := make([]string, maxCachedVectors+52)
	for i := range names {
		names[i] = fmt.Sprintf("playlist name %d", i)
	}

	if err := matcher.Warm(ctx, SourcePlaylists, names); err != nil {
		t.Fatalf("first Warm: %v", err)
	}
	if got := embedder.embeddedSince(0); got != len(names) {
		t.Fatalf("first warm embedded %d texts, want the whole library %d", got, len(names))
	}

	// Every later warm fills only what the bound could not keep.
	want := len(names) - maxCachedVectors
	for pass := 2; pass <= 3; pass++ {
		before := embedder.calls()
		if err := matcher.Warm(ctx, SourcePlaylists, names); err != nil {
			t.Fatalf("warm pass %d: %v", pass, err)
		}
		if got := embedder.embeddedSince(before); got != want {
			t.Fatalf("warm pass %d embedded %d texts, want %d: the cache cleared instead of evicting", pass, got, want)
		}
		matcher.mu.Lock()
		total := matcher.cache.total
		matcher.mu.Unlock()
		if total != maxCachedVectors {
			t.Fatalf("cached entries after warm pass %d = %d, want the bound %d", pass, total, maxCachedVectors)
		}
	}
}
