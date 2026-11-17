package speech

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean returns reply text ready for TTS: markdown emphasis, headings, code
// marks, list markers and emoji are removed, and line breaks become sentence
// spacing. It is the whole-string form of Cleaner.
func Clean(text string) string {
	c := NewCleaner()
	return c.Write(text) + c.Flush()
}

// Cleaner removes markdown and emoji from streamed reply text. It holds back a
// trailing line-start marker that may still be incomplete, so a marker split
// across deltas ("*" then "*bold**") is recognised rather than half-emitted.
type Cleaner struct {
	buf []byte
	out []byte
	// sent is how much of out has already been returned by Write.
	sent         int
	atLineStart  bool
	spacePending bool
}

// NewCleaner returns a cleaner positioned at the start of a line.
func NewCleaner() *Cleaner {
	return &Cleaner{atLineStart: true}
}

// Write appends a raw delta and returns the cleaned text that is now safe to
// speak. Text that may still change is held until the next Write or Flush.
func (c *Cleaner) Write(delta string) string {
	if delta == "" {
		return ""
	}
	c.buf = append(c.buf, delta...)
	c.drain(false)
	return c.take()
}

// Flush returns the remaining cleaned text and empties the held buffer.
func (c *Cleaner) Flush() string {
	c.drain(true)
	return c.take()
}

func (c *Cleaner) take() string {
	if c.sent >= len(c.out) {
		return ""
	}
	chunk := string(c.out[c.sent:])
	c.sent = len(c.out)
	return chunk
}

// drain consumes as much of buf as can be cleaned without more input. When
// final is false it keeps a possible incomplete line-start marker and a split
// rune for the next call.
func (c *Cleaner) drain(final bool) {
	for len(c.buf) > 0 {
		if c.atLineStart {
			rest := strings.TrimLeft(string(c.buf), " \t")
			if rest == "" {
				// Whitespace before any content on the line is not spoken.
				c.buf = nil
				return
			}
			// A fenced-code line ("```go") is a marker, not spoken text.
			if strings.HasPrefix(rest, "```") {
				if i := strings.IndexByte(rest, '\n'); i >= 0 {
					c.buf = []byte(rest[i+1:])
					continue
				}
				if !final {
					c.buf = []byte(rest)
					return
				}
				c.buf = nil
				return
			}
			if consumed, ok := markerPrefix(rest); ok {
				c.buf = []byte(rest[consumed:])
				c.atLineStart = false
				continue
			}
			if !final && couldBeMarker(rest) {
				// Hold "1", "-", "##" and the like until the next delta shows
				// whether it is a list/heading marker or ordinary text.
				c.buf = []byte(rest)
				return
			}
			c.buf = []byte(rest)
			c.atLineStart = false
		}
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			if final {
				c.emit(c.buf)
				c.buf = nil
				return
			}
			hold := incompleteRuneSuffix(c.buf)
			c.emit(c.buf[:len(c.buf)-hold])
			c.buf = append([]byte(nil), c.buf[len(c.buf)-hold:]...)
			return
		}
		line := c.buf[:i]
		c.buf = append([]byte(nil), c.buf[i+1:]...)
		if strings.TrimSpace(string(line)) == "" {
			// A blank line is paragraph spacing, not another sentence.
			continue
		}
		c.emit(line)
		c.newline()
		c.atLineStart = true
	}
}

// emit strips inline markers and emoji, normalises whitespace and appends the
// result. A space is deferred so output never ends in one.
func (c *Cleaner) emit(line []byte) {
	for len(line) > 0 {
		r, size := utf8.DecodeRune(line)
		line = line[size:]
		if r == '\r' {
			continue
		}
		if r == utf8.RuneError && size == 1 {
			// A split rune is held by drain; drop any stray byte defensively.
			continue
		}
		if unicode.IsSpace(r) {
			c.spacePending = true
			continue
		}
		if isMarkdownMarker(r) || isEmoji(r) {
			continue
		}
		c.writeRune(r)
	}
}

func (c *Cleaner) writeRune(r rune) {
	if c.spacePending && len(c.out) > 0 {
		c.out = append(c.out, ' ')
	}
	c.spacePending = false
	c.out = utf8.AppendRune(c.out, r)
}

// newline joins two lines as speech: a line that already ends in punctuation
// keeps a single space, anything else gains a full stop first.
func (c *Cleaner) newline() {
	if len(c.out) == 0 {
		return
	}
	if !endsWithPunctuation(c.out) {
		c.out = append(c.out, '.')
	}
	c.spacePending = true
}

func endsWithPunctuation(b []byte) bool {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		switch r {
		case '"', '\'', ')', ']', '}':
			b = b[:len(b)-size]
			continue
		}
		return strings.ContainsRune(".!?:;,", r)
	}
	return false
}

// markerPrefix reports how many bytes of s to drop when s starts with a line
// marker: a heading, a blockquote, or an unordered/numbered list item.
func markerPrefix(s string) (int, bool) {
	if n := countRun(s, '#'); n >= 1 && n <= 6 {
		if n < len(s) && isSpaceByte(s[n]) {
			return skipSpaces(s, n), true
		}
	}
	if strings.HasPrefix(s, ">") {
		return skipSpaces(s, 1), true
	}
	if s != "" && (s[0] == '-' || s[0] == '+' || s[0] == '*') {
		if len(s) > 1 && isSpaceByte(s[1]) {
			return skipSpaces(s, 1), true
		}
	}
	i := 0
	for i < len(s) && i < 3 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
		if i+1 < len(s) && isSpaceByte(s[i+1]) {
			return skipSpaces(s, i+1), true
		}
	}
	return 0, false
}

// couldBeMarker reports whether s is an incomplete prefix of a line marker, so
// drain should wait for the next delta before deciding.
func couldBeMarker(s string) bool {
	if strings.ContainsAny(s, "\n") {
		return false
	}
	t := strings.TrimRight(s, " \t")
	if t == "" {
		return true
	}
	switch t[0] {
	case '#':
		return len(t) <= 6 && countRun(t, '#') == len(t)
	case '>':
		return true
	case '-', '+', '*':
		return len(t) == 1
	}
	if t[0] >= '0' && t[0] <= '9' {
		i := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
		if i > 3 {
			return false
		}
		if i == len(t) {
			return true
		}
		return i+1 == len(t) && (t[i] == '.' || t[i] == ')')
	}
	return false
}

func countRun(s string, b byte) int {
	n := 0
	for n < len(s) && s[n] == b {
		n++
	}
	return n
}

func skipSpaces(s string, i int) int {
	for i < len(s) && isSpaceByte(s[i]) {
		i++
	}
	return i
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isMarkdownMarker(r rune) bool {
	switch r {
	case '*', '_', '~', '`', '#':
		return true
	}
	return false
}

// isEmoji covers the pictographic ranges and modifiers that reach replies.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // emoji, symbols, skin tones, flags
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols, dingbats
		return true
	case r >= 0x2B00 && r <= 0x2BFF:
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r == 0x200D: // zero-width joiner
		return true
	case r == 0x20E3: // combining enclosing keycap
		return true
	case r == 0x00A9 || r == 0x00AE || r == 0x2122: // © ® ™
		return true
	}
	return false
}

// incompleteRuneSuffix returns the byte length of a trailing UTF-8 sequence
// that is not complete yet.
func incompleteRuneSuffix(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return len(b) - i
			}
			return 0
		}
	}
	return 0
}
