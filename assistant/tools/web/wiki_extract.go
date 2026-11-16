package web

import (
	"container/list"
	"strings"
	"sync"
	"time"
	"unicode"
)

// wikiExamples are the utterances a user would actually say. They are the Fast
// path training phrases the semantic router indexes, and every one must also
// pass wikiExtract so a matched utterance actually fires the Tool.
var wikiExamples = []string{
	"who was Ada Lovelace",
	"who is Marie Curie",
	"who was Albert Einstein",
	"who is Isaac Newton",
	"who was Leonardo da Vinci",
	"what is a capybara",
	"what is an axiom",
	"what is a quasar",
}

// summaryCache is a bounded LRU of Wikipedia summaries with a fixed TTL. It is
// safe for concurrent use; an expired entry is dropped on read.
type summaryCache struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	capacity int
	ll       *list.List // front is most recently used
	items    map[string]*list.Element
}

type cacheElement struct {
	key     string
	out     WikiOut
	expires time.Time
}

// newSummaryCache builds an empty cache. A nil now uses time.Now.
func newSummaryCache(ttl time.Duration, capacity int, now func() time.Time) *summaryCache {
	if now == nil {
		now = time.Now
	}
	return &summaryCache{
		now:      now,
		ttl:      ttl,
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
	}
}

// get returns the cached summary for a topic and marks it recently used. An
// expired or absent entry returns false.
func (c *summaryCache) get(topic string) (WikiOut, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[topic]
	if !ok {
		return WikiOut{}, false
	}
	entry := elem.Value.(*cacheElement)
	if c.now().After(entry.expires) {
		c.remove(elem)
		return WikiOut{}, false
	}
	c.ll.MoveToFront(elem)
	return entry.out, true
}

// put stores or refreshes a summary, evicting the least recently used entry
// once the cache is full.
func (c *summaryCache) put(topic string, out WikiOut) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity <= 0 {
		return
	}
	if elem, ok := c.items[topic]; ok {
		entry := elem.Value.(*cacheElement)
		entry.out = out
		entry.expires = c.now().Add(c.ttl)
		c.ll.MoveToFront(elem)
		return
	}
	for c.ll.Len() >= c.capacity {
		if back := c.ll.Back(); back != nil {
			c.remove(back)
		} else {
			break
		}
	}
	entry := &cacheElement{key: topic, out: out, expires: c.now().Add(c.ttl)}
	c.items[topic] = c.ll.PushFront(entry)
}

// remove drops one element from both the list and the key map.
func (c *summaryCache) remove(elem *list.Element) {
	c.ll.Remove(elem)
	delete(c.items, elem.Value.(*cacheElement).key)
}

// wikiExtract fills wiki.summary's argument from a strict grammar: "who is/was X"
// or "what is a/an X". It refuses any question with words after the noun phrase
// ("what is the capital of France", "who is the president of the United States",
// "what is a capybara's habitat") so those stay on the Agent path.
func wikiExtract(text string) (WikiIn, bool) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return WikiIn{}, false
	}
	lower := strings.ToLower(raw)
	// STT adds a trailing full stop to every transcript; drop it so "who was
	// Ada Lovelace." still matches the prefix.
	lower = strings.TrimRight(lower, ".!? ")
	raw = strings.TrimRight(raw, ".!? ")

	var topic string
	for _, prefix := range []string{"who is ", "who was ", "who were ", "who's ", "what is a ", "what is an "} {
		if strings.HasPrefix(lower, prefix) {
			topic = strings.TrimSpace(raw[len(prefix):])
			break
		}
	}
	if topic == "" {
		return WikiIn{}, false
	}
	topic = strings.Trim(topic, " ?.!,")
	if topic == "" {
		return WikiIn{}, false
	}
	if wikiHasExtraWords(topic) {
		return WikiIn{}, false
	}
	return WikiIn{Topic: topic}, true
}

// wikiStopWords are the clause markers that mean the utterance asks for more
// than a one-line summary: a preposition, conjunction, relative word or
// possessive. A leading determiner ("the/a/an") is part of a noun phrase such as
// "the Beatles" and is deliberately not here, so "who is the president of the
// United States" is rejected by "of", not by "the".
var wikiStopWords = map[string]bool{
	"of": true, "in": true, "on": true, "for": true, "to": true, "with": true,
	"about": true, "by": true, "from": true, "at": true, "and": true, "or": true,
	"but": true, "my": true, "your": true, "our": true, "their": true, "this": true,
	"that": true, "these": true, "those": true, "whose": true, "which": true,
	"who": true, "what": true, "how": true, "why": true, "when": true, "where": true,
	"into": true, "onto": true, "over": true, "under": true, "as": true,
	"is": true, "are": true, "was": true, "were": true, "did": true, "does": true,
	"do": true,
}

// wikiHasExtraWords reports whether a noun phrase carries words that mean the
// original question asked for more than a one-line summary.
func wikiHasExtraWords(topic string) bool {
	if strings.Contains(topic, "'") {
		// A possessive ("capybara's habitat") is a multi-clause question.
		return true
	}
	for _, word := range strings.Fields(topic) {
		word = strings.TrimRight(strings.ToLower(word), ".!?,")
		if wikiStopWords[word] {
			return true
		}
	}
	return false
}

// firstTwoSentences returns the first two sentences of an extract, capped at
// maxRunes without cutting a word. Sentence boundaries ignore common
// abbreviations ("Dr.", "U.S.", "e.g.") so a summary such as "Dr. Smith was
// born in D.C." is not split mid-name.
func firstTwoSentences(text string, maxRunes int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	sentences := splitSentences(text)
	if len(sentences) > 2 {
		sentences = sentences[:2]
	}
	joined := strings.TrimSpace(strings.Join(sentences, " "))
	if maxRunes > 0 {
		joined = trimToRunes(joined, maxRunes)
	}
	return joined
}

// splitSentences walks the runes and returns sentence segments. A terminator
// (. ! ?) ends a sentence only when it is not an abbreviation and the next
// sentence starts with an uppercase letter, a digit or opening punctuation.
func splitSentences(text string) []string {
	runes := []rune(text)
	n := len(runes)
	var sentences []string
	var current strings.Builder
	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			sentences = append(sentences, s)
		}
		current.Reset()
	}
	for i := 0; i < n; i++ {
		r := runes[i]
		if r == '.' || r == '!' || r == '?' {
			if isAbbreviationAt(runes, i) {
				current.WriteRune(r)
				continue
			}
			current.WriteRune(r)
			j := i + 1
			for j < n && isClosingPunct(runes[j]) {
				current.WriteRune(runes[j])
				j++
			}
			for j < n && isSpaceRune(runes[j]) {
				current.WriteRune(runes[j])
				j++
			}
			if j >= n {
				flush()
				i = j - 1
				continue
			}
			next := runes[j]
			if unicode.IsUpper(next) || unicode.IsDigit(next) || isOpeningPunct(next) {
				flush()
				i = j - 1
				continue
			}
			i = j - 1
			continue
		}
		current.WriteRune(r)
	}
	flush()
	return sentences
}

// isAbbreviationAt reports whether the terminator at dot is part of an
// abbreviation rather than a sentence end. It catches both a single-token
// abbreviation ("Dr.", "etc.") and an initials run ("U.S.", "e.g.").
func isAbbreviationAt(runes []rune, dot int) bool {
	if dot == 0 {
		return false
	}
	// "X.X" initials: a letter after the dot that is itself followed by a dot.
	if dot+2 < len(runes) && isLetter(runes[dot+1]) && runes[dot+2] == '.' {
		return true
	}
	// Preceding token (letters and internal periods), cleaned, in the set.
	start := dot
	for start-1 >= 0 {
		c := runes[start-1]
		if isLetter(c) || c == '.' {
			start--
		} else {
			break
		}
	}
	token := strings.ReplaceAll(strings.ToUpper(string(runes[start:dot])), ".", "")
	return token != "" && wikiAbbreviations[token]
}

// wikiAbbreviations is the bounded set of tokens that may precede a period
// without ending a sentence.
var wikiAbbreviations = map[string]bool{
	"DR": true, "MR": true, "MRS": true, "MS": true, "PROF": true, "SR": true,
	"JR": true, "ST": true, "VS": true, "ETC": true, "EG": true, "IE": true,
	"CAPT": true, "GEN": true, "COL": true, "LT": true, "MAJ": true, "SGT": true,
	"REV": true, "HON": true, "ESQ": true, "INC": true, "LTD": true, "CO": true,
	"CORP": true, "PHD": true, "MD": true, "DDS": true, "DVM": true, "RN": true,
	"US": true, "UK": true, "UN": true, "EU": true, "DC": true, "ETAL": true,
	"AL": true,
}

// trimToRunes caps text at max runes on a word boundary, adding an ellipsis when
// it cuts. A single word longer than max is cut without an ellipsis as a last
// resort so the cap still holds.
func trimToRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	limit := max - 1 // leave room for the ellipsis
	end := limit
	for end > 0 && !isSpaceRune(runes[end-1]) {
		end--
	}
	if end == 0 {
		return string(runes[:limit])
	}
	return string(runes[:end]) + "…"
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func isClosingPunct(r rune) bool {
	return r == '"' || r == '”' || r == '’' || r == ')' || r == ']' || r == '}'
}

func isOpeningPunct(r rune) bool {
	return r == '"' || r == '“' || r == '‘' || r == '(' || r == '[' || r == '{'
}
