package speech

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// PronunciationKey is the live settings key and config.yaml field name.
const PronunciationKey = "speech.pronunciation"

// PronunciationDefinition registers the live speech.pronunciation setting.
func PronunciationDefinition(configValues []string) settings.Definition {
	opts := []settings.Option{
		settings.WithKind(settings.Live),
		settings.WithValidator(ValidatePronunciation),
		settings.WithDescription("Live pronunciation map for names and jargon: one entry per line, word=spoken. Longest key first on word boundaries."),
	}
	if len(configValues) > 0 {
		opts = append(opts, settings.WithConfig(configValues))
	}
	return settings.Strings(PronunciationKey, []string{}, opts...)
}

// ValidatePronunciation validates the speech.pronunciation list at write time.
// It skips blank lines, rejects lines without '=', lines with an empty key,
// lines with more than 60 runes on either side, and caps the list at 200 entries.
func ValidatePronunciation(value any) error {
	list, ok := value.([]string)
	if !ok {
		return fmt.Errorf("speech.pronunciation: want a list of strings, got %T", value)
	}
	count := 0
	for i, raw := range list {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		count++
		if count > 200 {
			return errors.New("speech.pronunciation: list exceeds 200 entries")
		}
		idx := strings.Index(trimmed, "=")
		if idx < 0 {
			return fmt.Errorf("speech.pronunciation: line %d %q: must contain '='", i+1, raw)
		}
		key := strings.TrimSpace(trimmed[:idx])
		if key == "" {
			return fmt.Errorf("speech.pronunciation: line %d %q: key must not be empty", i+1, raw)
		}
		val := strings.TrimSpace(trimmed[idx+1:])
		if len([]rune(key)) > 60 {
			return fmt.Errorf("speech.pronunciation: line %d: key exceeds 60 runes", i+1)
		}
		if len([]rune(val)) > 60 {
			return fmt.Errorf("speech.pronunciation: line %d: spoken text exceeds 60 runes", i+1)
		}
	}
	return nil
}

// Pronouncer performs whole-word, case-insensitive substitution on word boundaries.
type Pronouncer struct {
	entries []pronounceEntry
}

type pronounceEntry struct {
	key      string
	spoken   string
	keyRunes []rune
}

// NewPronouncer builds a Pronouncer from effective pronunciation entries.
// Blank and malformed lines are ignored, and at most 200 valid entries are loaded.
// Entries are ordered longest key first for greedy prefix matching.
func NewPronouncer(lines []string) *Pronouncer {
	var entries []pronounceEntry
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		idx := strings.Index(trimmed, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		if key == "" {
			continue
		}
		val := strings.TrimSpace(trimmed[idx+1:])
		keyRunes := []rune(key)
		valRunes := []rune(val)
		if len(keyRunes) > 60 || len(valRunes) > 60 {
			continue
		}
		entries = append(entries, pronounceEntry{
			key:      key,
			spoken:   val,
			keyRunes: keyRunes,
		})
		if len(entries) >= 200 {
			break
		}
	}

	// Longest key first so specific terms take precedence over prefixes.
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].keyRunes) > len(entries[j].keyRunes)
	})
	return &Pronouncer{entries: entries}
}

// Replace substitutes whole words according to the pronunciation map.
// Replacement is case-insensitive, longest key first, on word boundaries,
// and leaves the original word's case alone where possible.
func (p *Pronouncer) Replace(text string) string {
	if p == nil || len(p.entries) == 0 || text == "" {
		return text
	}
	runes := []rune(text)
	var b strings.Builder
	b.Grow(len(text))

	for i := 0; i < len(runes); {
		matched := false
		for _, e := range p.entries {
			kLen := len(e.keyRunes)
			if i+kLen > len(runes) {
				continue
			}
			// Left boundary: if key starts with a word rune, previous rune cannot be a word rune.
			if isWordRune(e.keyRunes[0]) {
				if i > 0 && isWordRune(runes[i-1]) {
					continue
				}
			}
			// Right boundary: if key ends with a word rune, next rune cannot be a word rune.
			if isWordRune(e.keyRunes[kLen-1]) {
				if i+kLen < len(runes) && isWordRune(runes[i+kLen]) {
					continue
				}
			}
			// Case-insensitive comparison under Unicode simple fold.
			sub := string(runes[i : i+kLen])
			if strings.EqualFold(sub, e.key) {
				b.WriteString(e.spoken)
				i += kLen
				matched = true
				break
			}
		}
		if !matched {
			b.WriteRune(runes[i])
			i++
		}
	}
	return b.String()
}

// isWordRune reports whether r is a word constituent (letter, number, or underscore).
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
