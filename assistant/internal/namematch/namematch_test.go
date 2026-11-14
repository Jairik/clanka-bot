package namematch_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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
