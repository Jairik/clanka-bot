package speech_test

import (
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

func TestBudgetStopsAtTheFirstBoundaryAfterTheLimit(t *testing.T) {
	cases := []struct {
		name     string
		limits   speech.Limits
		deltas   []string
		want     string
		reached  bool
		lastCall int
	}{
		{
			name:     "two sentences",
			limits:   speech.Limits{MaxSentences: 2, MaxChars: 1000},
			deltas:   []string{"One. Two. Three. Four."},
			want:     "One. Two.",
			reached:  true,
			lastCall: 0,
		},
		{
			name:     "character budget comes first",
			limits:   speech.Limits{MaxSentences: 10, MaxChars: 12},
			deltas:   []string{"Hello there. Second."},
			want:     "Hello there.",
			reached:  true,
			lastCall: 0,
		},
		{
			name:     "sentence split across deltas",
			limits:   speech.Limits{MaxSentences: 2, MaxChars: 1000},
			deltas:   []string{"One.", " Two.", " Three."},
			want:     "One. Two.",
			reached:  true,
			lastCall: 2,
		},
		{
			name:     "exactly at the character budget",
			limits:   speech.Limits{MaxSentences: 5, MaxChars: 4},
			deltas:   []string{"One. Two."},
			want:     "One.",
			reached:  true,
			lastCall: 0,
		},
		{
			name:     "no boundary yet",
			limits:   speech.Limits{MaxSentences: 2, MaxChars: 220},
			deltas:   []string{"Still talking"},
			want:     "Still talking",
			reached:  false,
			lastCall: -1,
		},
		{
			name:     "long single sentence has no boundary to cut",
			limits:   speech.Limits{MaxSentences: 1, MaxChars: 40},
			deltas:   []string{strings.Repeat("word ", 20) + "end"},
			want:     strings.Repeat("word ", 20) + "end",
			reached:  false,
			lastCall: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := speech.NewBudget(tc.limits)
			var spoken strings.Builder
			reachedAt := -1
			for i, delta := range tc.deltas {
				chunk, reached := budget.Write(delta)
				spoken.WriteString(chunk)
				if reached && reachedAt < 0 {
					reachedAt = i
				}
			}
			spoken.WriteString(budget.Flush())
			if got := spoken.String(); got != tc.want {
				t.Errorf("spoken = %q, want %q", got, tc.want)
			}
			if got := budget.Text(); got != tc.want {
				t.Errorf("Text() = %q, want %q", got, tc.want)
			}
			if tc.reached != (reachedAt >= 0) {
				t.Errorf("reached = %v, want %v", reachedAt >= 0, tc.reached)
			}
			if tc.reached && reachedAt != tc.lastCall {
				t.Errorf("reached at delta %d, want %d", reachedAt, tc.lastCall)
			}
			if tc.reached {
				if chunk, still := budget.Write("Later. More."); chunk != "" || !still {
					t.Errorf("Write after the cut = (%q, %v), want (\"\", true)", chunk, still)
				}
				if rest := budget.Flush(); rest != "" {
					t.Errorf("Flush after the cut = %q, want empty", rest)
				}
			}
		})
	}
}

func TestBudgetCleansBeforeCounting(t *testing.T) {
	budget := speech.NewBudget(speech.Limits{MaxSentences: 1, MaxChars: 220})
	chunk, _ := budget.Write("**One.** 😊 Two.")
	chunk += budget.Flush()
	if chunk != "One." {
		t.Fatalf("spoken = %q, want the cleaned first sentence only", chunk)
	}
}
