package ring

import "strings"

// SplitAudio separates a completion request before parsing its duration or clock.
func SplitAudio(text string) (string, string) {
	if i := strings.Index(strings.ToLower(text), " and play "); i >= 0 {
		return text[:i], strings.TrimSpace(text[i+10:])
	}
	return text, ""
}
