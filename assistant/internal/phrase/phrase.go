// Package phrase holds the one normalisation used for routing comparisons:
// the Control phrase table, the Agent request prefix and the semantic
// embedder must agree on what counts as the same words. It deliberately only
// trims trailing sentence punctuation; duration and other meaningful syntax
// stay untouched.
package phrase

import "strings"

// Normalize lowercases, trims, collapses whitespace and drops trailing
// sentence punctuation. Use it when case and a final full stop must not change
// a routing decision.
func Normalize(text string) string {
	return strings.ToLower(TrimSentence(text))
}

// TrimSentence trims, collapses whitespace and drops trailing sentence
// punctuation, preserving case. The semantic embedder uses it so an STT full
// stop cannot change the score while names and "I" keep their case.
func TrimSentence(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, "?.!")
	return strings.Join(strings.Fields(text), " ")
}
