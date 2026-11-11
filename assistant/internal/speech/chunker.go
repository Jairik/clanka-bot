package speech

import "strings"

// firstChunkWords is how many words the first chunk may hold before it is
// flushed even without punctuation, so speech starts during generation.
const firstChunkWords = 12

// clauseMinWords keeps an early clause flush from speaking a fragment such as
// "Sure," as its own chunk.
const clauseMinWords = 3

// Chunker turns streamed reply text into speakable chunks. The first chunk is
// flushed early, at a clause boundary or after about twelve words; later chunks
// end at sentence boundaries. Feed it model deltas with Write and finish with
// Flush.
type Chunker struct {
	buf   string
	first bool
}

// NewChunker returns an empty chunker with its early first chunk pending.
func NewChunker() *Chunker {
	return &Chunker{first: true}
}

// Write appends a delta and returns every chunk that is now complete.
func (c *Chunker) Write(delta string) []string {
	if delta == "" {
		return nil
	}
	c.buf += delta
	var chunks []string
	for {
		cut := c.cut()
		if cut <= 0 {
			return chunks
		}
		chunk := strings.TrimSpace(c.buf[:cut])
		c.buf = c.buf[cut:]
		c.first = false
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
}

// Flush returns the remaining text, if any, and empties the chunker.
func (c *Chunker) Flush() string {
	chunk := strings.TrimSpace(c.buf)
	c.buf = ""
	return chunk
}

// cut returns the index just past the earliest complete chunk, or 0.
func (c *Chunker) cut() int {
	best := sentenceCut(c.buf)
	if c.first {
		if n := clauseCut(c.buf); n > 0 && (best == 0 || n < best) {
			best = n
		}
		if n := wordCut(c.buf, firstChunkWords); n > 0 && (best == 0 || n < best) {
			best = n
		}
	}
	return best
}

// sentenceCut finds the first sentence end or line break. A trailing dot needs
// the next delta before it counts, so "3." can still become "3.14".
func sentenceCut(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			if strings.TrimSpace(s[:i]) != "" {
				return i + 1
			}
		case '!', '?':
			return skipClosers(s, i) + 1
		case '.':
			if endsSentence(s, i) {
				return skipClosers(s, i) + 1
			}
		}
	}
	return 0
}

// endsSentence reports whether the dot at i closes a sentence rather than an
// abbreviation, a decimal or a leading list marker.
func endsSentence(s string, i int) bool {
	if i+1 >= len(s) {
		return false
	}
	next := s[i+1]
	if isLetterOrDigit(next) || next == '.' {
		return false
	}
	if next != ' ' && next != '\t' && next != '\n' && !isCloser(next) {
		return false
	}
	if isListMarker(s, i) {
		return false
	}
	_, abbreviation := abbreviations[lastToken(s[:i])]
	return !abbreviation
}

// isListMarker reports whether the dot at i belongs to "1." or "a." at the
// start of a line, as in a numbered list.
func isListMarker(s string, i int) bool {
	token := lastToken(s[:i])
	if token == "" {
		return false
	}
	numeric := true
	for _, r := range token {
		if r < '0' || r > '9' {
			numeric = false
			break
		}
	}
	if !numeric && len(token) != 1 {
		return false
	}
	if !numeric && (token[0] < 'a' || token[0] > 'z') {
		return false
	}
	// The token must be the first thing on its line for it to be a list marker.
	return strings.TrimSpace(s[:i-len(token)]) == ""
}

// abbreviations are tokens whose dot does not end a sentence. Lowercase, and
// dotted forms keep their dots ("e.g", "a.m").
var abbreviations = map[string]struct{}{
	"mr": {}, "mrs": {}, "ms": {}, "dr": {}, "prof": {}, "sr": {}, "jr": {},
	"st": {}, "vs": {}, "etc": {}, "e.g": {}, "i.e": {}, "a.m": {}, "p.m": {},
	"approx": {}, "dept": {}, "est": {}, "inc": {}, "ltd": {},
	"fig": {}, "al": {}, "rd": {}, "ave": {}, "blvd": {}, "u.s": {}, "u.k": {},
}

// clauseCut flushes the first chunk at a clause boundary once it has enough
// words to be worth speaking.
func clauseCut(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ',', ';', ':':
			if len(strings.Fields(s[:i+1])) >= clauseMinWords {
				return i + 1
			}
		}
	}
	return 0
}

// wordCut flushes after the nth word, at the whitespace that follows it, so a
// delta split mid-word never leaks a broken word into TTS.
func wordCut(s string, n int) int {
	words := 0
	inWord := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n':
			if inWord {
				inWord = false
				if words == n {
					return i
				}
			}
		default:
			if !inWord {
				inWord = true
				words++
			}
		}
	}
	return 0
}

// lastToken returns the final whitespace-delimited word, lowercased and
// stripped of wrapping punctuation.
func lastToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.Trim(fields[len(fields)-1], "\"'()[]{}"))
}

func isLetterOrDigit(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func isCloser(b byte) bool {
	return b == '"' || b == '\'' || b == ')' || b == ']' || b == '}'
}

// skipClosers extends a sentence end over trailing quotes or brackets.
func skipClosers(s string, i int) int {
	for i+1 < len(s) && isCloser(s[i+1]) {
		i++
	}
	return i
}
