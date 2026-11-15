package namematch_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
)

// fakeEmbedder is the embed service boundary. It records every call so a test
// can prove an exact match never reaches the network.
type fakeEmbedder struct {
	calls int
	texts []string
	reply func(texts []string) ([][]float32, error)
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.texts = append([]string(nil), texts...)
	if f.reply == nil {
		return nil, fmt.Errorf("unexpected embed call for %v", texts)
	}
	return f.reply(texts)
}

// tableVectors returns one fixed vector per expected text.
func tableVectors(table map[string][]float32) func([]string) ([][]float32, error) {
	return func(texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			vec, ok := table[text]
			if !ok {
				return nil, fmt.Errorf("no fake vector for %q", text)
			}
			vectors[i] = vec
		}
		return vectors, nil
	}
}

// fixedThreshold makes the live threshold explicit in a test.
func fixedThreshold(value float64) func() float64 {
	return func() float64 { return value }
}

func TestExactAndNormalisedMatchesSkipTheEmbedder(t *testing.T) {
	embedder := &fakeEmbedder{}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})
	candidates := []string{"birds", "lo-fi beats", "Jarvis", "Morning Coffee"}

	cases := []struct {
		spoken string
		want   string
	}{
		{"birds", "birds"},
		{"  Birds. ", "birds"},
		{"lo fi beats", "lo-fi beats"},
		{"the birds sound", "birds"},
		{"my jarvis voice", "Jarvis"},
		{"Morning Coffee Playlist", "Morning Coffee"},
		{"the lo fi beats playlist", "lo-fi beats"},
	}
	for _, tc := range cases {
		t.Run(tc.spoken, func(t *testing.T) {
			result := matcher.Match(context.Background(), tc.spoken, candidates)
			if !result.Matched() || result.Name != tc.want {
				t.Fatalf("Match(%q) = %+v, want %q", tc.spoken, result, tc.want)
			}
			if !result.Exact || result.Score != 1 || result.Reason != namematch.ReasonExact {
				t.Fatalf("Match(%q) = %+v, want an exact score of 1", tc.spoken, result)
			}
		})
	}
	if embedder.calls != 0 {
		t.Fatalf("embedder called %d times for exact and normalised matches, want 0", embedder.calls)
	}
}

func TestSimilarityPicksTheBestCandidateAboveTheThreshold(t *testing.T) {
	// "bird" is 0.8 cosine from "birds" and unrelated to the other names.
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds":    {1, 0, 0},
		"jazz mix": {0, 0, 1},
		"rain":     {0, 1, 0},
		"bird":     {0.8, 0.6, 0},
	})}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.70)})

	result := matcher.Match(context.Background(), "the bird sound", []string{"rain", "birds", "jazz mix"})
	if !result.Matched() || result.Name != "birds" {
		t.Fatalf("Match = %+v, want birds", result)
	}
	if result.Exact || result.Reason != namematch.ReasonSimilarity {
		t.Fatalf("Match = %+v, want a similarity match", result)
	}
	if math.Abs(result.Score-0.8) > 1e-6 {
		t.Fatalf("score = %.6f, want 0.8", result.Score)
	}
	if embedder.calls != 1 {
		t.Fatalf("embed calls = %d, want 1", embedder.calls)
	}
	wantTexts := []string{"birds", "jazz mix", "rain", "bird"}
	if strings.Join(embedder.texts, "|") != strings.Join(wantTexts, "|") {
		t.Fatalf("embedded texts = %v, want the sorted candidates plus the normalised query %v", embedder.texts, wantTexts)
	}
}

func TestBelowThresholdReturnsNoMatch(t *testing.T) {
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 0},
		"rain":  {0, 1},
		"bird":  {0.5, -float32(math.Sqrt(0.75))},
	})}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.70)})

	result := matcher.Match(context.Background(), "the bird sound", []string{"birds", "rain"})
	if result.Matched() {
		t.Fatalf("Match = %+v, want no match below the threshold", result)
	}
	if result.Reason != namematch.ReasonBelowThreshold {
		t.Fatalf("reason = %q, want %q", result.Reason, namematch.ReasonBelowThreshold)
	}
	if math.Abs(result.Score-0.5) > 1e-6 {
		t.Fatalf("score = %.6f, want the best score 0.5 reported for logging", result.Score)
	}
}

func TestThresholdIsReadOnEveryMatch(t *testing.T) {
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 0},
		"bird":  {0.8, 0.6},
	})}
	limit := 0.70
	matcher := namematch.New(embedder, namematch.Config{Threshold: func() float64 { return limit }})
	candidates := []string{"birds"}

	if result := matcher.Match(context.Background(), "bird", candidates); result.Name != "birds" {
		t.Fatalf("at 0.70 Match = %+v, want birds", result)
	}
	limit = 0.90
	if result := matcher.Match(context.Background(), "bird", candidates); result.Matched() {
		t.Fatalf("at 0.90 Match = %+v, want no match", result)
	}
	if embedder.calls != 2 {
		t.Fatalf("embed calls = %d, want one per similarity lookup", embedder.calls)
	}
}

func TestEmptyInputsReturnNoMatchWithoutAnEmbedCall(t *testing.T) {
	embedder := &fakeEmbedder{}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})
	ctx := context.Background()

	if result := matcher.Match(ctx, "birds", nil); result.Matched() || result.Reason != namematch.ReasonNoCandidates {
		t.Fatalf("no candidates = %+v, want no_candidates", result)
	}
	if result := matcher.Match(ctx, "  ...  ", []string{"birds"}); result.Matched() || result.Reason != namematch.ReasonEmptyName {
		t.Fatalf("punctuation-only speech = %+v, want empty_name", result)
	}
	if result := matcher.Match(ctx, "birds", []string{"", "   ", "!!!"}); result.Matched() || result.Reason != namematch.ReasonNoCandidates {
		t.Fatalf("unspellable candidates = %+v, want no_candidates", result)
	}
	if embedder.calls != 0 {
		t.Fatalf("embed calls = %d, want 0 for empty input", embedder.calls)
	}
}

func TestDuplicateNamesAreDeterministic(t *testing.T) {
	embedder := &fakeEmbedder{}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})
	ctx := context.Background()

	// The same name twice is still one library entry, so it resolves.
	result := matcher.Match(ctx, "birds", []string{"birds", "birds", " rain "})
	if result.Name != "birds" || !result.Exact {
		t.Fatalf("exact duplicate = %+v, want birds", result)
	}

	// Different names that normalise to the same words cannot be told apart
	// by speech, so the matcher refuses to guess.
	ambiguous := []string{"Lo-Fi Beats", "Lo Fi-Beats"}
	result = matcher.Match(ctx, "lo fi beats", ambiguous)
	if result.Matched() || result.Reason != namematch.ReasonAmbiguous {
		t.Fatalf("duplicate normalised names = %+v, want ambiguous", result)
	}

	// Case-only duplicates are equally indistinguishable to speech.
	result = matcher.Match(ctx, "BIRDS", []string{"Birds", "birds"})
	if result.Matched() || result.Reason != namematch.ReasonAmbiguous {
		t.Fatalf("case-only duplicates = %+v, want ambiguous", result)
	}
	if embedder.calls != 0 {
		t.Fatalf("embed calls = %d, want 0 for ambiguous names", embedder.calls)
	}
}

func TestUnavailableEmbedderKeepsExactNamesWorking(t *testing.T) {
	matcher := namematch.New(nil, namematch.Config{Threshold: fixedThreshold(0.60)})
	ctx := context.Background()

	if result := matcher.Match(ctx, "the birds sound", []string{"birds"}); result.Name != "birds" {
		t.Fatalf("exact match without an embedder = %+v, want birds", result)
	}
	result := matcher.Match(ctx, "the bird sound", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonEmbedFailed || result.Err == nil {
		t.Fatalf("similarity without an embedder = %+v, want embed_failed with an error", result)
	}
}

func TestEmbedFailureReturnsNoMatch(t *testing.T) {
	embedder := &fakeEmbedder{reply: func([]string) ([][]float32, error) {
		return nil, errors.New("embed service is down")
	}}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})

	result := matcher.Match(context.Background(), "the bird sound", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonEmbedFailed {
		t.Fatalf("result = %+v, want embed_failed", result)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "down") {
		t.Fatalf("error = %v, want the embed failure preserved", result.Err)
	}
}

func TestMalformedVectorsReturnNoMatch(t *testing.T) {
	cases := map[string]func([]string) ([][]float32, error){
		"too few vectors": func([]string) ([][]float32, error) {
			return [][]float32{{1, 0}}, nil
		},
		"empty candidate vector": func([]string) ([][]float32, error) {
			return [][]float32{{}, {1, 0}}, nil
		},
		"empty query vector": func([]string) ([][]float32, error) {
			return [][]float32{{1, 0}, {}}, nil
		},
		"dimension mismatch": func([]string) ([][]float32, error) {
			return [][]float32{{1, 0}, {1, 0, 0}}, nil
		},
		"not a number": func([]string) ([][]float32, error) {
			return [][]float32{{float32(math.NaN()), 0}, {1, 0}}, nil
		},
		"infinity": func([]string) ([][]float32, error) {
			return [][]float32{{float32(math.Inf(1)), 0}, {1, 0}}, nil
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			matcher := namematch.New(&fakeEmbedder{reply: reply}, namematch.Config{Threshold: fixedThreshold(0.60)})
			result := matcher.Match(context.Background(), "the bird sound", []string{"birds"})
			if result.Matched() || result.Reason != namematch.ReasonInvalidVectors {
				t.Fatalf("result = %+v, want invalid_vectors", result)
			}
			if result.Err == nil {
				t.Fatal("malformed vectors must report the shape error")
			}
		})
	}
}

func TestCanceledContextReturnsNoMatch(t *testing.T) {
	embedder := &fakeEmbedder{reply: func(texts []string) ([][]float32, error) {
		return nil, context.Canceled
	}}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := matcher.Match(ctx, "the bird sound", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonEmbedFailed {
		t.Fatalf("result = %+v, want embed_failed", result)
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", result.Err)
	}

	// Exact matches need no embed call, so a canceled request can still resolve
	// one; that costs nothing and avoids a second lookup.
	if exact := matcher.Match(ctx, "birds", []string{"birds"}); exact.Name != "birds" {
		t.Fatalf("exact match with a canceled context = %+v, want birds", exact)
	}
}

func TestMatchTimeoutBoundsOneLookup(t *testing.T) {
	embedder := &fakeEmbedder{reply: func(texts []string) ([][]float32, error) {
		// The matcher must install a deadline smaller than the caller's.
		return nil, context.DeadlineExceeded
	}}
	matcher := namematch.New(embedder, namematch.Config{
		Threshold: fixedThreshold(0.60),
		Timeout:   time.Millisecond,
	})
	result := matcher.Match(context.Background(), "the bird sound", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonEmbedFailed {
		t.Fatalf("result = %+v, want embed_failed", result)
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"   ":                      "",
		"Birds":                    "birds",
		"  The Birds.  ":           "birds",
		"The Lo-Fi Beats Playlist": "lo fi beats",
		"my running playlist":      "running",
		"the bird sound":           "bird",
		"bird sounds":              "bird",
		"sound":                    "sound",
		"the sound":                "sound",
		"Lo-Fi Beats":              "lo fi beats",
		"Jarvis's voice":           "jarvis s",
		"!! birds !!!":             "birds",
	}
	for input, want := range cases {
		if got := namematch.Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestInvalidThresholdFallsBackToTheDefault(t *testing.T) {
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 0},
		"bird":  {0.7, float32(math.Sqrt(1 - 0.49))},
	})}
	for _, limit := range []float64{0, -0.5, 1.01, 2} {
		matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(limit)})
		result := matcher.Match(context.Background(), "bird", []string{"birds"})
		if result.Name != "birds" {
			t.Fatalf("threshold %v: Match = %+v, want the default 0.60 to apply", limit, result)
		}
	}
	// A nil threshold has the same fallback.
	matcher := namematch.New(embedder, namematch.Config{})
	if result := matcher.Match(context.Background(), "bird", []string{"birds"}); result.Name != "birds" {
		t.Fatalf("nil threshold: Match = %+v, want the default", result)
	}
}

func TestEqualTopScoresAreAmbiguous(t *testing.T) {
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 1},
		"rain":  {1, 1},
		"dusk":  {1, 1},
	})}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})

	result := matcher.Match(context.Background(), "dusk", []string{"birds", "rain"})
	if result.Matched() || result.Reason != namematch.ReasonAmbiguous {
		t.Fatalf("equal scores = %+v, want ambiguous rather than a coin flip", result)
	}
	if math.Abs(result.Score-1) > 1e-6 {
		t.Fatalf("score = %v, want the tied best score reported", result.Score)
	}
}

// batchRecorder records every Embed batch, not just the last, so a cache test
// can prove which texts reached the embed service on each call. It is safe for
// concurrent use.
type batchRecorder struct {
	mu      sync.Mutex
	batches [][]string
	table   map[string][]float32
	reply   func(texts []string) ([][]float32, error)
}

func (r *batchRecorder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]string(nil), texts...))
	table, reply := r.table, r.reply
	r.mu.Unlock()
	if reply != nil {
		return reply(texts)
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vec, ok := table[text]
		if !ok {
			return nil, fmt.Errorf("no fake vector for %q", text)
		}
		vectors[i] = vec
	}
	return vectors, nil
}

func (r *batchRecorder) setTable(table map[string][]float32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.table = table
}

func (r *batchRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func (r *batchRecorder) batch(i int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.batches[i]...)
}

func sameTexts(got, want []string) bool {
	return strings.Join(got, "|") == strings.Join(want, "|")
}

func TestRepeatedMatchReusesCachedCandidateVectors(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds":    {1, 0, 0},
		"jazz mix": {0, 0, 1},
		"bird":     {0.8, 0.6, 0},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	candidates := []string{"jazz mix", "birds"}

	first := matcher.Match(context.Background(), "the bird sound", candidates)
	if !first.Matched() || first.Name != "birds" {
		t.Fatalf("first Match = %+v, want birds", first)
	}
	if recorder.calls() != 1 {
		t.Fatalf("first Match made %d embed calls, want 1", recorder.calls())
	}
	if got := recorder.batch(0); !sameTexts(got, []string{"birds", "jazz mix", "bird"}) {
		t.Fatalf("first batch = %v, want the two candidates and the query", got)
	}

	second := matcher.Match(context.Background(), "the bird sound", candidates)
	if !second.Matched() || second.Name != "birds" || second.Score != first.Score {
		t.Fatalf("second Match = %+v, want the same result as %+v", second, first)
	}
	if recorder.calls() != 2 {
		t.Fatalf("second Match made %d embed calls in total, want 2", recorder.calls())
	}
	if got := recorder.batch(1); !sameTexts(got, []string{"bird"}) {
		t.Fatalf("second batch = %v, want exactly the query", got)
	}
}

func TestAddingOneCandidateEmbedsOnlyTheNewCandidateAndTheQuery(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds": {1, 0, 0},
		"rain":  {0, 1, 0},
		"storm": {0, 0, 1},
		"bird":  {0.8, 0.6, 0},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	ctx := context.Background()

	if result := matcher.Match(ctx, "the bird sound", []string{"birds", "rain"}); result.Name != "birds" {
		t.Fatalf("first Match = %+v, want birds", result)
	}
	if result := matcher.Match(ctx, "the bird sound", []string{"birds", "rain", "storm"}); !result.Matched() {
		t.Fatalf("second Match = %+v, want a match", result)
	}
	if recorder.calls() != 2 {
		t.Fatalf("embed calls = %d, want 2", recorder.calls())
	}
	if got := recorder.batch(1); !sameTexts(got, []string{"storm", "bird"}) {
		t.Fatalf("second batch = %v, want the new candidate and the query only", got)
	}
	// The cache is now complete, so a repeat lookup embeds only the query.
	if result := matcher.Match(ctx, "the bird sound", []string{"birds", "rain", "storm"}); result.Name != "birds" {
		t.Fatalf("third Match = %+v, want birds", result)
	}
	if got := recorder.batch(2); !sameTexts(got, []string{"bird"}) {
		t.Fatalf("third batch = %v, want only the query", got)
	}
}

func TestWarmFillsTheCacheSoTheNextMatchEmbedsOnlyTheQuery(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds":    {1, 0},
		"jazz mix": {0, 1},
		"bird":     {0.8, 0.6},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	ctx := context.Background()
	candidates := []string{"Jazz Mix", "birds"}

	if err := matcher.Warm(ctx, candidates); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if recorder.calls() != 1 {
		t.Fatalf("Warm made %d embed calls, want 1", recorder.calls())
	}
	if got := recorder.batch(0); !sameTexts(got, []string{"jazz mix", "birds"}) {
		t.Fatalf("warm batch = %v, want both normalised candidates", got)
	}

	result := matcher.Match(ctx, "the bird sound", candidates)
	if !result.Matched() || result.Name != "birds" {
		t.Fatalf("Match after Warm = %+v, want birds", result)
	}
	if recorder.calls() != 2 {
		t.Fatalf("embed calls = %d, want 2: Warm then the query only", recorder.calls())
	}
	if got := recorder.batch(1); !sameTexts(got, []string{"bird"}) {
		t.Fatalf("match batch = %v, want only the query", got)
	}
}

func TestWarmMakesNoEmbedRequestWhenEverythingIsCached(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds": {1, 0},
		"rain":  {0, 1},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	ctx := context.Background()
	candidates := []string{"birds", "rain"}

	if err := matcher.Warm(ctx, candidates); err != nil {
		t.Fatalf("first Warm: %v", err)
	}
	if err := matcher.Warm(ctx, candidates); err != nil {
		t.Fatalf("second Warm: %v", err)
	}
	if recorder.calls() != 1 {
		t.Fatalf("embed calls = %d, want 1: the second Warm is a no-op", recorder.calls())
	}
	if recorder.calls() > 0 && len(recorder.batch(0)) != 2 {
		t.Fatalf("warm batch = %v, want both candidates", recorder.batch(0))
	}
}

func TestWarmWithNoEmbedderIsANoOp(t *testing.T) {
	matcher := namematch.New(nil, namematch.Config{Threshold: fixedThreshold(0.70)})
	if err := matcher.Warm(context.Background(), []string{"birds"}); err != nil {
		t.Fatalf("Warm without an embedder = %v, want nil", err)
	}
}

// A cold start (no refresh has happened yet, so Warm never ran) must keep
// working: the first lookup embeds the candidates and the query once, as it
// always did, and leaves the cache complete for the warm that follows.
func TestFirstLookupResolvesWithoutAWarm(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds":    {1, 0},
		"jazz mix": {0, 1},
		"bird":     {0.8, 0.6},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	ctx := context.Background()
	candidates := []string{"Jazz Mix", "birds"}

	result := matcher.Match(ctx, "the bird sound", candidates)
	if !result.Matched() || result.Name != "birds" || result.Reason != namematch.ReasonSimilarity {
		t.Fatalf("cold first Match = %+v, want birds by similarity", result)
	}
	if recorder.calls() != 1 {
		t.Fatalf("cold first Match made %d embed calls, want 1", recorder.calls())
	}
	if got := recorder.batch(0); !sameTexts(got, []string{"jazz mix", "birds", "bird"}) {
		t.Fatalf("cold first batch = %v, want every candidate plus the query", got)
	}

	// The lookup already filled the cache, so the warm that a refresh would
	// now trigger has nothing to embed.
	if err := matcher.Warm(ctx, candidates); err != nil {
		t.Fatalf("Warm after the cold lookup: %v", err)
	}
	if recorder.calls() != 1 {
		t.Fatalf("embed calls after a post-lookup Warm = %d, want 1", recorder.calls())
	}
}

func TestModelDimensionChangeDiscardsAndReembedsCachedVectors(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds": {1, 0},
		"rain":  {0, 1},
		"bird":  {0.8, 0.6},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	ctx := context.Background()
	candidates := []string{"birds", "rain"}

	// Fill the cache against the two-dimension model. "birds" wins at 0.8.
	if result := matcher.Match(ctx, "the bird sound", candidates); result.Name != "birds" {
		t.Fatalf("two-dimension Match = %+v, want birds", result)
	}

	// The embed model is redeployed with a third dimension, where "rain" is
	// the true nearest. A stale two-dimension vector would either score
	// nonsense or panic, so every candidate must be embedded again.
	recorder.setTable(map[string][]float32{
		"birds": {0, 1, 0},
		"rain":  {1, 0, 0},
		"bird":  {1, 0, 0},
	})
	result := matcher.Match(ctx, "the bird sound", candidates)
	if !result.Matched() || result.Name != "rain" {
		t.Fatalf("three-dimension Match = %+v, want rain", result)
	}
	if recorder.calls() != 3 {
		t.Fatalf("embed calls = %d, want 3: the query then the stale candidates", recorder.calls())
	}
	if got := recorder.batch(1); !sameTexts(got, []string{"bird"}) {
		t.Fatalf("second batch = %v, want only the query against the new model", got)
	}
	if got := recorder.batch(2); !sameTexts(got, []string{"birds", "rain"}) {
		t.Fatalf("re-embed batch = %v, want every stale candidate", got)
	}

	// The refilled cache now holds three-dimension vectors, so a repeat lookup
	// embeds only the query.
	if result := matcher.Match(ctx, "the bird sound", candidates); result.Name != "rain" {
		t.Fatalf("repeat Match = %+v, want rain", result)
	}
	if got := recorder.batch(3); !sameTexts(got, []string{"bird"}) {
		t.Fatalf("repeat batch = %v, want only the query", got)
	}
}

func TestConcurrentMatchesAndWarmShareTheCacheSafely(t *testing.T) {
	recorder := &batchRecorder{table: map[string][]float32{
		"birds": {1, 0},
		"rain":  {0, 1},
		"dusk":  {0, 1},
		"bird":  {0.8, 0.6},
	}}
	matcher := namematch.New(recorder, namematch.Config{Threshold: fixedThreshold(0.70)})
	candidates := []string{"birds", "rain", "dusk"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if result := matcher.Match(context.Background(), "the bird sound", candidates); result.Name != "birds" {
					t.Errorf("concurrent Match = %+v, want birds", result)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			if err := matcher.Warm(context.Background(), candidates); err != nil {
				t.Errorf("concurrent Warm: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
