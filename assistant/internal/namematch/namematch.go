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

// DefaultTimeout bounds one lookup, including embedding every candidate.
// Callers with a shorter context deadline keep their own deadline.
const DefaultTimeout = 2 * time.Second

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

// Matcher resolves spoken names against caller-supplied candidates. It holds
// no per-library state: each lookup is independent, so sounds, voices and
// playlists can share one matcher without a cache to refresh or lock.
type Matcher struct {
	embedder  Embedder
	threshold func() float64
	timeout   time.Duration
}

// New builds a matcher over the shared embed client. A nil embedder is allowed
// for callers that only need exact matching; a similarity lookup then returns
// no match with ReasonEmbedFailed.
func New(embedder Embedder, cfg Config) *Matcher {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Matcher{embedder: embedder, threshold: cfg.Threshold, timeout: timeout}
}

// Match resolves spoken against candidates. Candidates are original library
// names, not paths or IDs. The call makes no embed request when an exact or
// normalised name resolves.
func (m *Matcher) Match(ctx context.Context, spoken string, candidates []string) Result {
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
	type candidate struct{ name, text string }
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

	// 3. One batch embeds the candidates and the query together. Libraries are
	// small (sounds, voices, one user's playlists), so this is simpler than a
	// vector cache whose staleness would need its own refresh policy.
	texts := make([]string, 0, len(indexed)+1)
	for _, item := range indexed {
		texts = append(texts, item.text)
	}
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

	queryVector := vectors[len(indexed)]
	best, bestScore, ties := -1, math.Inf(-1), 0
	for i := range indexed {
		score := vector.Cosine(queryVector, vectors[i])
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
