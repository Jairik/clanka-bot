package phrase_test

import (
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
)

// TestNormalizeFoldsTrailingPunctuationAndTypographicApostrophes pins the one
// normalisation the Control phrase table, the Agent request prefix and the
// clear-yes set share (#191).
func TestNormalizeFoldsTrailingPunctuationAndTypographicApostrophes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Trailing sentence punctuation, including a comma and ellipsis.
		{"full stop", "stop.", "stop"},
		{"comma", "stop,", "stop"},
		{"semicolon", "stop;", "stop"},
		{"colon", "stop:", "stop"},
		{"ellipsis", "i'm awake…", "i'm awake"},
		{"three dots", "i'm awake...", "i'm awake"},
		{"stacked marks", "stop?!", "stop"},
		{"mark then space", "stop ,", "stop"},
		// Closing quotes and brackets, which desktop and phone keyboards add.
		{"closing straight double quote", `stop"`, "stop"},
		{"closing straight single quote", "stop'", "stop"},
		{"closing curly double quote", "stop”", "stop"},
		{"closing curly single quote", "stop’", "stop"},
		{"closing guillemet", "«stop»", "«stop"},
		{"closing parenthesis", "stop)", "stop"},
		{"closing bracket", "stop]", "stop"},
		{"punctuation inside closing quote", `stop,"`, "stop"},
		// Only the end is trimmed: no stored phrase opens with a bare quote, so
		// a wrapped transcript is left alone rather than half-stripped.
		{"leading quote kept", "“stop”", "“stop"},
		{"leading quote kept, straight", `"stop"`, `"stop`},
		// Typographic apostrophes fold onto the ASCII one stored phrases use.
		{"curly apostrophe", "what’s the time", "what's the time"},
		{"modifier letter apostrophe", "whatʼs the time", "what's the time"},
		{"fullwidth apostrophe", "what＇s the time", "what's the time"},
		{"curly apostrophe inside a phrase", "i’m awake", "i'm awake"},
		// An internal comma is meaningful, so it survives; the clear-yes lookup
		// tolerates it instead of the normaliser deleting it.
		{"internal comma", "yes, please", "yes, please"},
		{"comma without a space", "yes,please", "yes,please"},
		// Meaningful syntax must keep its shape.
		{"decimal duration", "1.5 hours", "1.5 hours"},
		{"ampersand label keeps its shape", "R&B", "r&b"},
		{"slash label keeps its shape", "AC/DC", "ac/dc"},
		{"clock time", "1:30", "1:30"},
		{"apostrophe inside a name", "O'Brien", "o'brien"},
		{"trailing decimal point", "3.", "3"},
		// Trimming stays at the end: a leading quote is left for the caller.
		{"blank", "   ", ""},
		{"collapsed whitespace", "  set\ta\ttimer  ", "set a timer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := phrase.Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTrimSentenceKeepsCase covers the embedder input: it shares the trimming
// and apostrophe rules but must not lowercase, so names keep their signal.
func TestTrimSentenceKeepsCase(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"I'm Awake.", "I'm Awake"},
		{"What’s the time?", "What's the time"},
		{"play AC/DC,", "play AC/DC"},
		{"  Set a timer for 1.5 hours.  ", "Set a timer for 1.5 hours"},
		{"play R&B,", "play R&B"},
		{"play AC/DC.", "play AC/DC"},
		{"call O’Brien", "call O'Brien"},
	}
	for _, tc := range cases {
		if got := phrase.TrimSentence(tc.in); got != tc.want {
			t.Errorf("TrimSentence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
