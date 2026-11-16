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
			result := matcher.Match(context.Background(), namematch.SourceSounds, tc.spoken, candidates)
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

	result := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", []string{"rain", "birds", "jazz mix"})
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

	result := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", []string{"birds", "rain"})
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

func TestBelowThresholdWithinMarginReturnsANearCandidateButNoMatch(t *testing.T) {
	// "bird" scores 0.55 against "birds", below the 0.60 floor but within
	// NearMargin, so the closest name is offered without becoming a match.
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 0},
		"bird":  {0.55, 0.8351646544245033}, // unit vector: cosine 0.55 with birds
	})}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})

	result := matcher.Match(context.Background(), namematch.SourceSounds, "bird", []string{"birds"})
	if result.Matched() || result.Name != "" {
		t.Fatalf("Match = %+v, want no match (Near must not become a match)", result)
	}
	if result.Reason != namematch.ReasonBelowThreshold {
		t.Fatalf("reason = %q, want %q", result.Reason, namematch.ReasonBelowThreshold)
	}
	if result.Near != "birds" {
		t.Fatalf("Near = %q, want %q", result.Near, "birds")
	}
	if math.Abs(result.Score-0.55) > 1e-6 {
		t.Fatalf("score = %.6f, want 0.55", result.Score)
	}
}

func TestBelowThresholdBeyondMarginReturnsNoNearCandidate(t *testing.T) {
	// 0.40 is more than NearMargin (0.10) below the 0.60 floor: a clear miss
	// must not suggest a name.
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 0},
		"bird":  {0.4, 0.916515138991168}, // unit vector: cosine 0.40 with birds
	})}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})

	result := matcher.Match(context.Background(), namematch.SourceSounds, "bird", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonBelowThreshold {
		t.Fatalf("Match = %+v, want a below-threshold miss", result)
	}
	if result.Near != "" {
		t.Fatalf("Near = %q, want empty for a far miss", result.Near)
	}
}

func TestAmbiguousExactTieReturnsNoNearCandidate(t *testing.T) {
	// Two names score equally well: speech cannot choose, so no question is
	// offered even though both are within reach of the threshold.
	embedder := &fakeEmbedder{reply: tableVectors(map[string][]float32{
		"birds": {1, 0},
		"rain":  {0, 1},
		"dusk":  {1, 1},
	})}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})

	result := matcher.Match(context.Background(), namematch.SourceSounds, "dusk", []string{"birds", "rain"})
	if result.Matched() || result.Reason != namematch.ReasonAmbiguous {
		t.Fatalf("Match = %+v, want an ambiguous tie", result)
	}
	if result.Near != "" {
		t.Fatalf("Near = %q, want empty for an ambiguous tie", result.Near)
	}
}

func TestNearQuestionSharesTheWording(t *testing.T) {
	if got, want := namematch.NearQuestion("birds", "sound"), "I couldn't find that. Did you mean the birds sound?"; got != want {
		t.Fatalf("NearQuestion = %q, want %q", got, want)
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

	if result := matcher.Match(context.Background(), namematch.SourceSounds, "bird", candidates); result.Name != "birds" {
		t.Fatalf("at 0.70 Match = %+v, want birds", result)
	}
	limit = 0.90
	if result := matcher.Match(context.Background(), namematch.SourceSounds, "bird", candidates); result.Matched() {
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

	if result := matcher.Match(ctx, namematch.SourceSounds, "birds", nil); result.Matched() || result.Reason != namematch.ReasonNoCandidates {
		t.Fatalf("no candidates = %+v, want no_candidates", result)
	}
	if result := matcher.Match(ctx, namematch.SourceSounds, "  ...  ", []string{"birds"}); result.Matched() || result.Reason != namematch.ReasonEmptyName {
		t.Fatalf("punctuation-only speech = %+v, want empty_name", result)
	}
	if result := matcher.Match(ctx, namematch.SourceSounds, "birds", []string{"", "   ", "!!!"}); result.Matched() || result.Reason != namematch.ReasonNoCandidates {
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
	result := matcher.Match(ctx, namematch.SourceSounds, "birds", []string{"birds", "birds", " rain "})
	if result.Name != "birds" || !result.Exact {
		t.Fatalf("exact duplicate = %+v, want birds", result)
	}

	// Different names that normalise to the same words cannot be told apart
	// by speech, so the matcher refuses to guess.
	ambiguous := []string{"Lo-Fi Beats", "Lo Fi-Beats"}
	result = matcher.Match(ctx, namematch.SourceSounds, "lo fi beats", ambiguous)
	if result.Matched() || result.Reason != namematch.ReasonAmbiguous {
		t.Fatalf("duplicate normalised names = %+v, want ambiguous", result)
	}

	// Case-only duplicates are equally indistinguishable to speech.
	result = matcher.Match(ctx, namematch.SourceSounds, "BIRDS", []string{"Birds", "birds"})
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

	if result := matcher.Match(ctx, namematch.SourceSounds, "the birds sound", []string{"birds"}); result.Name != "birds" {
		t.Fatalf("exact match without an embedder = %+v, want birds", result)
	}
	result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonEmbedFailed || result.Err == nil {
		t.Fatalf("similarity without an embedder = %+v, want embed_failed with an error", result)
	}
}

func TestEmbedFailureReturnsNoMatch(t *testing.T) {
	embedder := &fakeEmbedder{reply: func([]string) ([][]float32, error) {
		return nil, errors.New("embed service is down")
	}}
	matcher := namematch.New(embedder, namematch.Config{Threshold: fixedThreshold(0.60)})

	result := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", []string{"birds"})
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
			result := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", []string{"birds"})
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

	result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", []string{"birds"})
	if result.Matched() || result.Reason != namematch.ReasonEmbedFailed {
		t.Fatalf("result = %+v, want embed_failed", result)
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", result.Err)
	}

	// Exact matches need no embed call, so a canceled request can still resolve
	// one; that costs nothing and avoids a second lookup.
	if exact := matcher.Match(ctx, namematch.SourceSounds, "birds", []string{"birds"}); exact.Name != "birds" {
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
	result := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", []string{"birds"})
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
		result := matcher.Match(context.Background(), namematch.SourceSounds, "bird", []string{"birds"})
		if result.Name != "birds" {
			t.Fatalf("threshold %v: Match = %+v, want the default 0.60 to apply", limit, result)
		}
	}
	// A nil threshold has the same fallback.
	matcher := namematch.New(embedder, namematch.Config{})
	if result := matcher.Match(context.Background(), namematch.SourceSounds, "bird", []string{"birds"}); result.Name != "birds" {
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

	result := matcher.Match(context.Background(), namematch.SourceSounds, "dusk", []string{"birds", "rain"})
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
	times   []time.Time
	table   map[string][]float32
	reply   func(texts []string) ([][]float32, error)
}

func (r *batchRecorder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]string(nil), texts...))
	r.times = append(r.times, time.Now())
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

func (r *batchRecorder) timeAt(i int) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.times[i]
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

	first := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", candidates)
	if !first.Matched() || first.Name != "birds" {
		t.Fatalf("first Match = %+v, want birds", first)
	}
	if recorder.calls() != 1 {
		t.Fatalf("first Match made %d embed calls, want 1", recorder.calls())
	}
	if got := recorder.batch(0); !sameTexts(got, []string{"birds", "jazz mix", "bird"}) {
		t.Fatalf("first batch = %v, want the two candidates and the query", got)
	}

	second := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", candidates)
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

	if result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", []string{"birds", "rain"}); result.Name != "birds" {
		t.Fatalf("first Match = %+v, want birds", result)
	}
	if result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", []string{"birds", "rain", "storm"}); !result.Matched() {
		t.Fatalf("second Match = %+v, want a match", result)
	}
	if recorder.calls() != 2 {
		t.Fatalf("embed calls = %d, want 2", recorder.calls())
	}
	if got := recorder.batch(1); !sameTexts(got, []string{"storm", "bird"}) {
		t.Fatalf("second batch = %v, want the new candidate and the query only", got)
	}
	// The cache is now complete, so a repeat lookup embeds only the query.
	if result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", []string{"birds", "rain", "storm"}); result.Name != "birds" {
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

	if err := matcher.Warm(ctx, namematch.SourceSounds, candidates); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if recorder.calls() != 1 {
		t.Fatalf("Warm made %d embed calls, want 1", recorder.calls())
	}
	if got := recorder.batch(0); !sameTexts(got, []string{"jazz mix", "birds"}) {
		t.Fatalf("warm batch = %v, want both normalised candidates", got)
	}

	result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", candidates)
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

	if err := matcher.Warm(ctx, namematch.SourceSounds, candidates); err != nil {
		t.Fatalf("first Warm: %v", err)
	}
	if err := matcher.Warm(ctx, namematch.SourceSounds, candidates); err != nil {
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
	if err := matcher.Warm(context.Background(), namematch.SourceSounds, []string{"birds"}); err != nil {
		t.Fatalf("Warm without an embedder = %v, want nil", err)
	}
}

// warmLibrary builds a large set of normalised names and a fake vector table
// for them, so a batching test can count exactly what reached the service.
func warmLibrary(n int) (candidates []string, table map[string][]float32) {
	candidates = make([]string, n)
	table = make(map[string][]float32, n)
	for i := range candidates {
		candidates[i] = fmt.Sprintf("track %03d", i)
		table[namematch.Normalize(candidates[i])] = []float32{float32(i%7) + 1, 0, 0}
	}
	return candidates, table
}

// TestWarmBatchesLargeLibraryAndStaysWithinBound is the reviewer's probe for
// issue #375: a 1000-name warm must be split into bounded requests, one per
// chunk, each text embedded exactly once, and still finish inside the bound.
func TestWarmBatchesLargeLibraryAndStaysWithinBound(t *testing.T) {
	const names = 1000
	candidates, table := warmLibrary(names)
	recorder := &batchRecorder{table: table}
	matcher := namematch.New(recorder, namematch.Config{})

	start := time.Now()
	if err := matcher.Warm(context.Background(), namematch.SourceSounds, candidates); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if elapsed := time.Since(start); elapsed > namematch.WarmTimeout {
		t.Fatalf("Warm took %v, want within %v", elapsed, namematch.WarmTimeout)
	}

	wantCalls := (names + namematch.WarmBatchSize - 1) / namematch.WarmBatchSize
	if got := recorder.calls(); got != wantCalls {
		t.Fatalf("Warm made %d embed calls, want %d: one per chunk of %d", got, wantCalls, namematch.WarmBatchSize)
	}
	seen := make(map[string]bool, names)
	for i := 0; i < recorder.calls(); i++ {
		batch := recorder.batch(i)
		if len(batch) == 0 || len(batch) > namematch.WarmBatchSize {
			t.Fatalf("batch %d has %d texts, want between 1 and %d", i, len(batch), namematch.WarmBatchSize)
		}
		for _, text := range batch {
			if seen[text] {
				t.Fatalf("text %q was embedded in more than one batch", text)
			}
			seen[text] = true
		}
	}
	if len(seen) != names {
		t.Fatalf("Warm embedded %d distinct texts, want %d", len(seen), names)
	}
}

// TestWarmYieldsBetweenBatches proves the warm pauses between chunks so a
// 1-text embed on the shared service is never queued behind the whole library.
func TestWarmYieldsBetweenBatches(t *testing.T) {
	candidates, table := warmLibrary(3 * namematch.WarmBatchSize)
	recorder := &batchRecorder{table: table}
	matcher := namematch.New(recorder, namematch.Config{})

	if err := matcher.Warm(context.Background(), namematch.SourceSounds, candidates); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if recorder.calls() < 2 {
		t.Fatalf("Warm made %d embed calls, want more than one", recorder.calls())
	}
	for i := 1; i < recorder.calls(); i++ {
		gap := recorder.timeAt(i).Sub(recorder.timeAt(i - 1))
		if gap < namematch.WarmBatchPause {
			t.Fatalf("gap between batch %d and %d = %v, want at least the %v yield", i-1, i, gap, namematch.WarmBatchPause)
		}
	}
}

// cancelAfterFirstEmbed cancels the warm once its first batch has been served,
// standing in for a caller that loses interest mid-warm.
type cancelAfterFirstEmbed struct {
	cancel context.CancelFunc
	calls  int
}

func (e *cancelAfterFirstEmbed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0}
	}
	if e.calls == 1 {
		e.cancel()
	}
	return vectors, nil
}

// A cancelled context stops the warm between batches; the remaining misses
// are left uncached rather than embedded after the caller has gone away.
func TestWarmStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	embedder := &cancelAfterFirstEmbed{cancel: cancel}
	matcher := namematch.New(embedder, namematch.Config{})
	candidates, _ := warmLibrary(2 * namematch.WarmBatchSize)

	err := matcher.Warm(ctx, namematch.SourceSounds, candidates)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Warm with cancelled context = %v, want context.Canceled", err)
	}
	if embedder.calls != 1 {
		t.Fatalf("Warm made %d embed calls after cancellation, want 1", embedder.calls)
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

	result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", candidates)
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
	if err := matcher.Warm(ctx, namematch.SourceSounds, candidates); err != nil {
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
	if result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", candidates); result.Name != "birds" {
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
	result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", candidates)
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
	if result := matcher.Match(ctx, namematch.SourceSounds, "the bird sound", candidates); result.Name != "rain" {
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
				if result := matcher.Match(context.Background(), namematch.SourceSounds, "the bird sound", candidates); result.Name != "birds" {
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
			if err := matcher.Warm(context.Background(), namematch.SourceSounds, candidates); err != nil {
				t.Errorf("concurrent Warm: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
