package pipeline

import (
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
)

// secondCommandClause reports whether a configured joiner introduces another
// request, so a semantic match must not execute only the first half. It is
// deliberately conservative:
//
//   - the words before the joiner must be a clause of at least two words;
//   - the words after it must be a whole Control phrase / Focus intent, or
//     start with a word that opens a Fast-path Example (a verb such as "set",
//     "tell" or "pause");
//   - a schedule continuation ("end at 7") is not a command.
//
// That keeps "an hour and a half", "tuesdays and thursdays" and a label with
// "and" on the Fast path while routing "set a timer and pause" or a repeated
// command to the Agent path.
func (p *Pipeline) secondCommandClause(text string, phrases, intents map[string]string) bool {
	if p.semantic == nil {
		return false
	}
	joiners := make(map[string]bool, len(p.semantic.Joiners()))
	for _, joiner := range p.semantic.Joiners() {
		if key := strings.ToLower(strings.TrimSpace(joiner)); key != "" {
			joiners[key] = true
		}
	}
	if len(joiners) == 0 {
		return false
	}
	matches := wordRE.FindAllStringIndex(strings.ToLower(text), -1)
	for i, span := range matches {
		// A joiner needs a clause before it and words after it.
		if i < 2 || i == len(matches)-1 {
			continue
		}
		if !joiners[strings.ToLower(text[span[0]:span[1]])] {
			continue
		}
		suffix := text[span[1]:]
		normalized := phrase.Normalize(suffix)
		if normalized == "" {
			continue
		}
		if _, ok := phrases[normalized]; ok {
			return true
		}
		if _, ok := intents[normalized]; ok {
			return true
		}
		if p.semantic.ClauseStartsCommand(suffix) {
			return true
		}
	}
	return false
}
