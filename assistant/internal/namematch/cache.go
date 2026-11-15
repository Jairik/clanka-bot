package namematch

import (
	"container/list"
)

// Source names the library a candidate belongs to. Candidate vectors are
// cached per source, so warming or filling one library cannot discard another
// library's vectors wholesale.
type Source string

// The libraries that resolve spoken names through one shared Matcher. Any
// other value shares the fallback partition, so a typo cannot mint an
// unbounded set of caches.
const (
	SourcePlaylists Source = "playlists"
	SourceListItems Source = "list_items"
	SourceSounds    Source = "sounds"
	SourceVoices    Source = "voices"
	SourcePersonas  Source = "personas"

	sourceFallback Source = "other"
)

// maxCachedVectors bounds the candidate vector cache at about 3 MB for a
// 384-dimension model: 2048 entries of 384 float32 values. The budget is
// shared by every source rather than split up front. A full cache gives way
// one vector at a time, starting with the least recently used vector of the
// source that holds the most, so a warm cannot evict another library's
// vectors, a library larger than the whole budget keeps its most recently
// used vectors instead of clearing, and repeated fills reach a steady state
// instead of oscillating.
const maxCachedVectors = 2048

// cacheKey identifies one cached vector: the source it belongs to and the
// normalised text that was embedded.
type cacheKey struct {
	source Source
	text   string
}

// cacheEntry is one cached vector. It is the value of a recency element.
type cacheEntry struct {
	key cacheKey
	vec []float32
}

// candidateCache is a bounded LRU of candidate vectors, partitioned by source.
// It is not safe for concurrent use; a Matcher guards it with its mutex.
type candidateCache struct {
	// dims is the model dimension the cache was filled with. It is zero while
	// the cache is empty.
	dims int
	// total is the number of cached vectors across every source.
	total int
	// byKey finds the recency element of one vector.
	byKey map[cacheKey]*list.Element
	// order holds one recency list per source that has entries, most recently
	// used at the front. A source with no entries has no list.
	order map[Source]*list.List
}

// newCandidateCache returns an empty cache.
func newCandidateCache() *candidateCache {
	return &candidateCache{
		byKey: make(map[cacheKey]*list.Element),
		order: make(map[Source]*list.List),
	}
}

// partition maps any source onto one of the bounded partitions. An unknown
// value shares the fallback so the entry bound cannot be bypassed.
func (c *candidateCache) partition(source Source) Source {
	switch source {
	case SourcePlaylists, SourceListItems, SourceSounds, SourceVoices, SourcePersonas:
		return source
	default:
		return sourceFallback
	}
}

// get returns the cached vector for one text and marks it as recently used.
// Stored vectors are never mutated, so the caller may hold the slice after the
// lock is released.
func (c *candidateCache) get(source Source, text string) ([]float32, bool) {
	key := cacheKey{source: c.partition(source), text: text}
	elem, ok := c.byKey[key]
	if !ok {
		return nil, false
	}
	c.order[key.source].MoveToFront(elem)
	return elem.Value.(*cacheEntry).vec, true
}

// lookup is get for a caller that can only use a vector of the current model
// dimension. A vector cached under another model is a miss, because scoring it
// would mix vector lengths.
func (c *candidateCache) lookup(source Source, text string) ([]float32, bool) {
	vec, ok := c.get(source, text)
	if !ok || (c.dims != 0 && len(vec) != c.dims) {
		return nil, false
	}
	return vec, true
}

// store records freshly embedded candidate vectors, keyed by the source and
// text that were embedded. A batch whose count does not match its texts is
// ignored, and a batch of another model dimension replaces the whole cache:
// every stored vector is unusable after a model change.
func (c *candidateCache) store(source Source, texts []string, vectors [][]float32, dims int) {
	if len(texts) == 0 || dims <= 0 || len(vectors) != len(texts) {
		return
	}
	if c.dims != 0 && c.dims != dims {
		c.reset()
	}
	c.dims = dims
	for i, text := range texts {
		c.put(source, text, vectors[i])
	}
}

// put stores one vector, evicting least recently used vectors while the cache
// is full. Storing a text that is already cached replaces it in place.
func (c *candidateCache) put(source Source, text string, vec []float32) {
	source = c.partition(source)
	key := cacheKey{source: source, text: text}
	if elem, ok := c.byKey[key]; ok {
		elem.Value.(*cacheEntry).vec = vec
		c.order[source].MoveToFront(elem)
		return
	}
	for c.total >= maxCachedVectors {
		if !c.evictOne(source) {
			break
		}
	}
	if c.order[source] == nil {
		c.order[source] = list.New()
	}
	elem := c.order[source].PushFront(&cacheEntry{key: key, vec: vec})
	c.byKey[key] = elem
	c.total++
}

// evictOne drops the least recently used vector of the source that holds the
// most vectors. inserting wins a tie, so a fill never evicts from a source
// that holds no more vectors than the one being filled, and two sources of
// equal size cannot evict each other in turn. It reports whether it removed
// anything.
func (c *candidateCache) evictOne(inserting Source) bool {
	var largest Source
	largestCount := 0
	for source, order := range c.order {
		count := order.Len()
		if count == 0 {
			continue
		}
		if count > largestCount || (count == largestCount && source < largest) {
			// The lexicographic tie-break keeps eviction deterministic.
			largest, largestCount = source, count
		}
	}
	if largestCount == 0 {
		return false
	}
	// The source being filled gives way when it is one of the largest, so a
	// source can only lose vectors to a strictly larger one.
	if order := c.order[inserting]; order != nil && order.Len() == largestCount {
		largest = inserting
	}
	order := c.order[largest]
	elem := order.Back()
	if elem == nil {
		return false
	}
	entry := elem.Value.(*cacheEntry)
	order.Remove(elem)
	delete(c.byKey, entry.key)
	c.total--
	if order.Len() == 0 {
		delete(c.order, largest)
	}
	return true
}

// reset empties the cache, for example when the embed model's dimension
// changes and every stored vector has become unusable.
func (c *candidateCache) reset() {
	c.byKey = make(map[cacheKey]*list.Element)
	c.order = make(map[Source]*list.List)
	c.total = 0
	c.dims = 0
}
