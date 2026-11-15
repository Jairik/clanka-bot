// Package namematch resolves a spoken name, such as a sound, voice or Spotify
// playlist name, against one library's candidates. Exact and normalised
// spellings win before any network call; otherwise MiniLM embedding similarity
// picks the closest candidate above a live threshold. A match is never forced:
// empty input, ambiguous duplicates, malformed vectors, an unavailable embed
// service and low scores all return no name.
package namematch

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/vector"
)

// Embedder is the embed service boundary. *embed.Client satisfies it.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// DefaultThreshold is the cosine floor used when no live setting is available.
// MiniLM scores a genuine near miss such as "the bird sound" -> "birds" around
// 0.85 after normalisation, while unrelated requests stay below 0.45.
const DefaultThreshold = 0.60

// DefaultTimeout bounds one lookup, including embedding any candidate vector
// that is not cached yet. Callers with a shorter context deadline keep their
// own deadline.
const DefaultTimeout = 2 * time.Second

// WarmTimeout bounds one background warm. It is generous because a warm runs
// off the request path; a caller's shorter deadline still applies.
const WarmTimeout = 2 * time.Minute

// Result reasons, reported in the request log rather than spoken.
const (
	ReasonExact          = "exact"
	ReasonSimilarity     = "similarity"
	ReasonNoCandidates   = "no_candidates"
	ReasonEmptyName      = "empty_name"
	ReasonAmbiguous      = "ambiguous"
	ReasonBelowThreshold = "below_threshold"
	ReasonEmbedFailed    = "embed_failed"
	ReasonInvalidVectors = "invalid_vectors"
)

// Result is one name resolution. Name is empty whenever the lookup did not
// match. Score is 1 for an exact or normalised match and the best cosine
// otherwise; it stays set for a below-threshold or ambiguous result so the
// caller can log how close the attempt was.
type Result struct {
	Name   string
	Score  float64
	Exact  bool
	Reason string
	Err    error
}

// Matched reports whether a candidate name was resolved.
func (r Result) Matched() bool { return r.Name != "" }

// Config tunes one Matcher.
type Config struct {
	// Threshold returns the live cosine floor. It is read per match, so a
	// Dashboard change applies on the next lookup without a second settings
	// observer. Nil or a value outside (0, 1] uses DefaultThreshold.
	Threshold func() float64
	// Timeout bounds one lookup. Zero or negative uses DefaultTimeout.
	Timeout time.Duration
}

// Matcher resolves spoken names against caller-supplied candidates. One
// matcher serves every library: candidate vectors are cached by the source
// that owns them and the normalised text that was embedded, so sounds, voices,
// playlists and list items share one matcher and one embed client without
// sharing a cache budget in a way that lets one library evict another. The
// query is never cached because it changes per request.
type Matcher struct {
	embedder  Embedder
	threshold func() float64
	timeout   time.Duration

	// mu guards cache. It is never held across an Embed call: hits are copied
	// out, the misses are embedded, then the lock is taken again to store
	// them.
	mu sync.Mutex
	// cache holds candidate vectors per source, keyed by the normalised text
	// that was embedded. An embedding is a pure function of that text and the
	// model, so an entry can only go stale when the embed model itself
	// changes. A model change that keeps the same dimension cannot be detected
	// here: the key carries no model identity, and the vectors are still valid
	// shapes.
	cache *candidateCache
}

// candidate is one library name and the normalised text embedded for it.
type candidate struct{ name, text string }

// New builds a matcher over the shared embed client. A nil embedder is allowed
// for callers that only need exact matching; a similarity lookup then returns
// no match with ReasonEmbedFailed.
func New(embedder Embedder, cfg Config) *Matcher {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Matcher{
		embedder:  embedder,
		threshold: cfg.Threshold,
		timeout:   timeout,
		cache:     newCandidateCache(),
	}
}

// Match resolves spoken against one source's candidates. Candidates are
// original library names, not paths or IDs. The call makes no embed request
// when an exact or normalised name resolves.
func (m *Matcher) Match(ctx context.Context, source Source, spoken string, candidates []string) Result {
	if len(candidates) == 0 {
		return Result{Reason: ReasonNoCandidates}
	}
	query := Normalize(spoken)
	if query == "" {
		return Result{Reason: ReasonEmptyName}
	}
	names := uniqueNames(candidates)

	// 1. Exact spelling ignoring case and surrounding space. It is the
	// strongest signal and needs no embedding call.
	var raw []string
	text := strings.TrimSpace(spoken)
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), text) {
			raw = append(raw, name)
		}
	}
	if len(raw) == 1 {
		return Result{Name: raw[0], Score: 1, Exact: true, Reason: ReasonExact}
	}
	if len(raw) > 1 {
		// Two different candidates are spelled the same apart from case, so
		// speech cannot tell them apart. Never pick one at random.
		return Result{Reason: ReasonAmbiguous}
	}

	// 2. Normalised spelling: punctuation, case, articles and a trailing
	// medium noun cannot change an exact match ("The Lo-Fi Beats Playlist").
	indexed := make([]candidate, 0, len(names))
	var normal []string
	for _, name := range names {
		text := Normalize(name)
		if text == "" {
			// A name made only of punctuation cannot be spoken; skip it rather
			// than let it score noise against every query.
			continue
		}
		indexed = append(indexed, candidate{name: name, text: text})
		if text == query {
			normal = append(normal, name)
		}
	}
	if len(normal) == 1 {
		return Result{Name: normal[0], Score: 1, Exact: true, Reason: ReasonExact}
	}
	if len(normal) > 1 {
		// Different names normalise to the same words (for example "Lo-Fi
		// Beats" and "Lo Fi Beats"). The spoken form cannot choose between
		// them, so this is a no-match rather than a forced guess.
		return Result{Reason: ReasonAmbiguous}
	}
	if len(indexed) == 0 {
		return Result{Reason: ReasonNoCandidates}
	}
	if m == nil || m.embedder == nil {
		return Result{Reason: ReasonEmbedFailed, Err: fmt.Errorf("namematch: no embedder configured")}
	}

	// 3. Candidate vectors are content-addressed by the normalised text and
	// the source that owns them, so a repeated lookup embeds only the query
	// plus any candidate it has not seen. The vectors are copied out under the
	// lock and the lock is released before the embed call.
	hits := m.cachedVectors(source, indexed)
	texts := make([]string, 0, len(indexed)+1)
	positions := make([]int, 0, len(indexed))
	for i, item := range indexed {
		if hits[i] == nil {
			texts = append(texts, item.text)
			positions = append(positions, i)
		}
	}
	queryPos := len(texts)
	texts = append(texts, query)

	embedCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	vectors, err := m.embedder.Embed(embedCtx, texts)
	if err != nil {
		return Result{Reason: ReasonEmbedFailed, Err: err}
	}
	if err := validateVectors(vectors, len(texts)); err != nil {
		return Result{Reason: ReasonInvalidVectors, Err: err}
	}
	queryVector := vectors[queryPos]
	dims := len(queryVector)

	// A vector cached under a previous model can have a different length;
	// vector.Cosine would read past the shorter one, so discard and re-embed
	// those candidates rather than score them.
	var stale []int
	for i, vec := range hits {
		if vec != nil && len(vec) != dims {
			stale = append(stale, i)
		}
	}
	if len(stale) > 0 {
		staleTexts := make([]string, len(stale))
		for j, i := range stale {
			staleTexts[j] = indexed[i].text
		}
		fresh, err := m.embedder.Embed(embedCtx, staleTexts)
		if err != nil {
			return Result{Reason: ReasonEmbedFailed, Err: err}
		}
		if err := validateVectors(fresh, len(staleTexts)); err != nil {
			return Result{Reason: ReasonInvalidVectors, Err: err}
		}
		for j, i := range stale {
			if len(fresh[j]) != dims {
				return Result{Reason: ReasonInvalidVectors, Err: fmt.Errorf(
					"namematch: vector for %q has %d dimensions, want %d", indexed[i].text, len(fresh[j]), dims)}
			}
			hits[i] = fresh[j]
		}
		m.storeCached(source, staleTexts, fresh, dims)
	}

	missTexts := make([]string, len(positions))
	for j, i := range positions {
		hits[i] = vectors[j]
		missTexts[j] = indexed[i].text
	}
	m.storeCached(source, missTexts, vectors[:len(positions)], dims)

	best, bestScore, ties := -1, math.Inf(-1), 0
	for i := range indexed {
		score := vector.Cosine(queryVector, hits[i])
		switch {
		case score > bestScore:
			best, bestScore, ties = i, score, 1
		case score == bestScore:
			// Exact ties are ambiguous: two names are equally close and the
			// spoken words cannot choose. Near-ties are not special-cased; the
			// threshold is the only tuning knob this matcher promises.
			ties++
		}
	}
	if best < 0 || math.IsNaN(bestScore) {
		return Result{Reason: ReasonInvalidVectors, Err: fmt.Errorf("namematch: no finite score")}
	}
	if bestScore < m.thresholdValue() {
		return Result{Score: bestScore, Reason: ReasonBelowThreshold}
	}
	if ties > 1 {
		return Result{Score: bestScore, Reason: ReasonAmbiguous}
	}
	return Result{Name: indexed[best].name, Score: bestScore, Reason: ReasonSimilarity}
}

// Warm embeds and caches every candidate of one source the cache is missing. A
// library owner calls it after a refresh so the next spoken lookup embeds only
// the query instead of a cold batch that can exceed one lookup's budget. It
// makes no embed request when everything is already cached. A warm never
// evicts another source's vectors unless this source grows to be the largest
// holder, so a warm of one library cannot discard another library's work. A
// concurrent Warm and Match may embed the same miss once each; that is
// harmless.
func (m *Matcher) Warm(ctx context.Context, source Source, candidates []string) error {
	if m == nil || m.embedder == nil {
		// Without an embedder there is nothing to warm; exact matching still
		// works, so a warm is a no-op rather than an error.
		return nil
	}
	names := uniqueNames(candidates)
	texts := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		text := Normalize(name)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		texts = append(texts, text)
	}
	if len(texts) == 0 {
		return nil
	}

	m.mu.Lock()
	misses := make([]string, 0, len(texts))
	if m.cache != nil {
		for _, text := range texts {
			if _, ok := m.cache.lookup(source, text); ok {
				continue
			}
			misses = append(misses, text)
		}
	} else {
		misses = append(misses, texts...)
	}
	m.mu.Unlock()
	if len(misses) == 0 {
		return nil
	}

	warmCtx, cancel := context.WithTimeout(ctx, WarmTimeout)
	defer cancel()
	vectors, err := m.embedder.Embed(warmCtx, misses)
	if err != nil {
		return err
	}
	if err := validateVectors(vectors, len(misses)); err != nil {
		return err
	}
	m.storeCached(source, misses, vectors, len(vectors[0]))
	return nil
}

// cachedVectors returns the cached vector per indexed candidate, nil for a
// miss. The lock is released before returning: stored vectors are never
// mutated, so a reader can safely score a shared slice.
func (m *Matcher) cachedVectors(source Source, indexed []candidate) [][]float32 {
	hits := make([][]float32, len(indexed))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		return hits
	}
	for i, item := range indexed {
		if vec, ok := m.cache.get(source, item.text); ok {
			hits[i] = vec
		}
	}
	return hits
}

// storeCached records freshly embedded candidate vectors, keyed by the source
// and the text that was embedded. The cache is bounded, and a store that does
// not fit evicts least recently used vectors rather than clearing the whole
// cache, so memory cannot grow with a runaway library and a warm of one
// source stays useful.
func (m *Matcher) storeCached(source Source, texts []string, vectors [][]float32, dims int) {
	if len(texts) == 0 || dims <= 0 || len(vectors) != len(texts) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		m.cache = newCandidateCache()
	}
	m.cache.store(source, texts, vectors, dims)
}

// Normalize folds a spoken or library name to the words compared before the
// embedding call: lower case, Unicode letters and digits only, one space
// between words, no leading article or possessive ("the", "a", "an", "my") and
// no trailing medium noun ("sound", "voice", "playlist" and plurals). So
// "The Birds Sound" and "birds" are the same name, while "the bird sound" and
// "birds" remain a near miss for the embedding step.
func Normalize(text string) string {
	cleaned := cleanWords(text)
	if cleaned == "" {
		return ""
	}
	words := strings.Fields(cleaned)
	if len(words) > 1 && leadingWords[words[0]] {
		words = words[1:]
	}
	if len(words) > 1 && trailingWords[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	if len(words) == 0 {
		return cleaned
	}
	return strings.Join(words, " ")
}

// cleanWords lowercases and keeps letters and digits, replacing every other
// rune with one space. Apostrophes and hyphens therefore become word breaks,
// which matches how the embed model tokenises "lo-fi".
func cleanWords(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := true
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// leadingWords are spoken filler before a name, not part of it.
var leadingWords = map[string]bool{"the": true, "a": true, "an": true, "my": true}

// trailingWords name the library medium; the same matcher serves all three,
// so dropping them keeps "the birds sound" and "birds" close.
var trailingWords = map[string]bool{
	"sound": true, "sounds": true,
	"voice": true, "voices": true,
	"playlist": true, "playlists": true,
}

// uniqueNames removes exact duplicates and sorts, so the embedded batch order
// and the tie scan are deterministic and library order can never change a
// result.
func uniqueNames(candidates []string) []string {
	seen := make(map[string]bool, len(candidates))
	names := make([]string, 0, len(candidates))
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateVectors rejects a response the matcher cannot score: the wrong
// count, empty or inconsistent dimensions, or a non-finite value.
func validateVectors(vectors [][]float32, want int) error {
	if len(vectors) != want {
		return fmt.Errorf("namematch: got %d vectors for %d texts", len(vectors), want)
	}
	dims := 0
	for i, vec := range vectors {
		if len(vec) == 0 {
			return fmt.Errorf("namematch: vector %d is empty", i)
		}
		if dims == 0 {
			dims = len(vec)
		} else if len(vec) != dims {
			return fmt.Errorf("namematch: vector %d has %d dimensions, want %d", i, len(vec), dims)
		}
		for _, value := range vec {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("namematch: vector %d is not finite", i)
			}
		}
	}
	return nil
}

// thresholdValue reads the live floor, falling back to the default for a nil
// or out-of-range value so a bad setting cannot make every name match.
func (m *Matcher) thresholdValue() float64 {
	if m == nil || m.threshold == nil {
		return DefaultThreshold
	}
	value := m.threshold()
	if value <= 0 || value > 1 {
		return DefaultThreshold
	}
	return value
}
