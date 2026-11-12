package namematch_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
)

// TestNameMatchAgainstMiniLM exercises the real model. It skips without
// ROUTING_EVAL_EMBED_URL, so the normal suite stays offline; the sweep that
// runs the routing evaluation also runs this table.
func TestNameMatchAgainstMiniLM(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("ROUTING_EVAL_EMBED_URL"))
	if url == "" {
		t.Skip("set ROUTING_EVAL_EMBED_URL to the MiniLM embed service to run the real name table")
	}
	matcher := namematch.New(embed.NewClient(url, "sentence-transformers/all-MiniLM-L6-v2"), namematch.Config{
		Threshold: func() float64 { return namematch.DefaultThreshold },
	})
	// One library covering the three kinds of spoken name: sounds, voices and
	// playlists. The matcher is deliberately shared, so the table proves the
	// same rules hold for each.
	candidates := []string{
		"birds", "rain", "chime", "thunder", "ocean waves",
		"jarvis", "alba",
		"jazz mix", "running", "morning coffee", "bedtime stories", "lo-fi beats",
	}
	cases := []struct {
		kind   string
		spoken string
		want   string
		exact  bool
	}{
		// Sounds: only "bird" vs "birds" needs the embedding step; the other
		// spellings resolve exactly after normalisation.
		{"sound", "the bird sound", "birds", false},
		{"sound", "the rain sounds", "rain", true},
		{"sound", "the chime sound", "chime", true},
		// Voices.
		{"voice", "the jarvis voice", "jarvis", true},
		{"voice", "jarvis voice please", "jarvis", false},
		// Playlists.
		{"playlist", "my running playlist", "running", true},
		{"playlist", "the jazz playlist", "jazz mix", false},
		{"playlist", "morning coffee playlist", "morning coffee", true},
		{"playlist", "the bedtime story playlist", "bedtime stories", false},
		{"playlist", "lo fi beats", "lo-fi beats", true},
		// Unrelated requests must never resolve to a library name.
		{"unrelated", "what is the weather tomorrow", "", false},
		{"unrelated", "set a timer for five minutes", "", false},
		{"unrelated", "turn up the volume", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.kind+"/"+tc.spoken, func(t *testing.T) {
			result := matcher.Match(context.Background(), tc.spoken, candidates)
			if result.Err != nil {
				t.Fatalf("Match(%q) error: %v", tc.spoken, result.Err)
			}
			if tc.want == "" {
				if result.Matched() {
					t.Fatalf("Match(%q) = %q (%.4f, %s), want no match", tc.spoken, result.Name, result.Score, result.Reason)
				}
				return
			}
			if result.Name != tc.want {
				t.Fatalf("Match(%q) = %q (%.4f, %s), want %q", tc.spoken, result.Name, result.Score, result.Reason, tc.want)
			}
			if result.Exact != tc.exact {
				t.Errorf("Match(%q).Exact = %v, want %v (reason %s)", tc.spoken, result.Exact, tc.exact, result.Reason)
			}
			if result.Score < namematch.DefaultThreshold {
				t.Errorf("Match(%q) score %.4f is below the threshold", tc.spoken, result.Score)
			}
		})
	}
}
