package speech

import "strings"

// Limits is the spoken-length budget for one reply: at most MaxSentences
// sentences or about MaxChars characters, whichever comes first.
type Limits struct {
	MaxSentences int
	MaxChars     int
}

// DefaultLimits is the Agent path's default spoken budget.
func DefaultLimits() Limits {
	return Limits{MaxSentences: 2, MaxChars: 220}
}

// Budget limits streamed reply text as it arrives. Feed raw deltas to Write;
// it returns the cleaned text that may be spoken now and reports when the
// budget has just been reached. Once reached, the caller must stop the stream:
// no later text is part of what the user hears.
type Budget struct {
	limits  Limits
	cleaner *Cleaner
	// pending is cleaned text not yet spoken, including a trailing dot that
	// still needs the next delta to prove it ended a sentence.
	pending strings.Builder
	text    strings.Builder
	chars   int
	// sentences counts spoken sentence boundaries.
	sentences int
	done      bool
}

// NewBudget returns a budget with zero limits replaced by DefaultLimits.
func NewBudget(limits Limits) *Budget {
	if limits.MaxSentences <= 0 {
		limits.MaxSentences = DefaultLimits().MaxSentences
	}
	if limits.MaxChars <= 0 {
		limits.MaxChars = DefaultLimits().MaxChars
	}
	return &Budget{limits: limits, cleaner: NewCleaner()}
}

// Write appends one raw delta. The returned text is cleaned and safe to speak.
// reached is true once the budget has been hit; from then on Write returns
// empty text so the caller can cancel the stream.
func (b *Budget) Write(delta string) (text string, reached bool) {
	if b.done {
		return "", true
	}
	return b.consume(b.cleaner.Write(delta), true)
}

// Text is the cleaned text spoken so far.
func (b *Budget) Text() string { return b.text.String() }

// Flush speaks the remaining held text unless the budget already cut the
// reply. It is the last call for one stream.
func (b *Budget) Flush() string {
	if b.done {
		return ""
	}
	text, _ := b.consume(b.cleaner.Flush(), false)
	return text
}

func (b *Budget) consume(cleaned string, stream bool) (string, bool) {
	b.pending.WriteString(cleaned)
	var out strings.Builder
	for {
		s := b.pending.String()
		cut := sentenceCut(s)
		if cut <= 0 {
			break
		}
		sentence := s[:cut]
		b.pending.Reset()
		b.pending.WriteString(s[cut:])
		b.sentences++
		out.WriteString(sentence)
		b.chars += len(sentence)
		if b.sentences >= b.limits.MaxSentences || b.chars >= b.limits.MaxChars {
			b.done = true
			b.pending.Reset()
			b.text.WriteString(out.String())
			return out.String(), true
		}
	}

	s := b.pending.String()
	b.pending.Reset()
	// While streaming, a trailing dot may still turn out to be part of a
	// decimal or abbreviation, so hold it until the next delta proves it.
	if hold := trailingDotHold(s); stream && hold > 0 {
		b.pending.WriteString(s[len(s)-hold:])
		s = s[:len(s)-hold]
	}
	out.WriteString(s)
	b.chars += len(s)
	b.text.WriteString(out.String())
	return out.String(), false
}

// trailingDotHold returns the byte length of a trailing full stop plus any
// closing punctuation, or 0 when the text has none.
func trailingDotHold(s string) int {
	i := len(s) - 1
	for i >= 0 && isCloser(s[i]) {
		i--
	}
	if i >= 0 && s[i] == '.' {
		return len(s) - i
	}
	return 0
}
