// Package phrase holds the one normalisation used for routing comparisons:
// the Control phrase table, the Agent request prefix and the semantic
// embedder must agree on what counts as the same words. It trims trailing
// sentence punctuation and folds typographic apostrophes; duration and other
// meaningful syntax inside the sentence stay untouched.
package phrase

import "strings"

// trailingMarks are the characters a transcript may end with: ASCII sentence
// marks, the comma, colon and ellipsis dictation adds, and the closing quotes
// or brackets a phone keyboard or STT supplies. Only the end is trimmed, so a
// duration ("1.5 hours"), a label ("R&B") or a name ("O'Brien") keeps its
// internal punctuation.
const trailingMarks = "?.!,;:\u2026\"'\u201D\u2019\u00BB)]"

// typographicApostrophes folds the apostrophes STT and phone keyboards produce
// onto the ASCII one stored phrases use, so "what’s" and "what's" agree.
var typographicApostrophes = strings.NewReplacer(
	"\u2019", "'",
	"\u02BC", "'",
	"\uFF07", "'",
)

// Normalize lowercases, trims, collapses whitespace, folds typographic
// apostrophes and drops trailing sentence punctuation. Use it when case and a
// final full stop must not change a routing decision.
func Normalize(text string) string {
	return strings.ToLower(TrimSentence(text))
}

// TrimSentence trims, collapses whitespace, folds typographic apostrophes and
// drops trailing sentence punctuation, preserving case. The semantic embedder
// uses it so an STT full stop or curly quote cannot change the score while
// names and "I" keep their case.
func TrimSentence(text string) string {
	text = typographicApostrophes.Replace(text)
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, trailingMarks)
	return strings.Join(strings.Fields(text), " ")
}
