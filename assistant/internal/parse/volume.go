package parse

import (
	"regexp"
)

// volumeToRe marks an absolute level: a number introduced by "to" or "at"
// (e.g. "down to 20", "volume at 80") is a target level, not a relative step.
// The number itself may be a digit or a spoken word ("to twenty"), which is why
// the detector re-parses the remainder with Number rather than a digit-only
// pattern.
var volumeToRe = regexp.MustCompile(`(?i)\b(?:to|at)\b`)

// volumeUpRe / volumeDownRe name the direction words a volume request may use.
var (
	volumeUpRe   = regexp.MustCompile(`(?i)\b(?:up|louder|raise|higher|increase)\b`)
	volumeDownRe = regexp.MustCompile(`(?i)\b(?:down|quieter|softer|lower|decrease|reduce)\b`)
)

// VolumeChange reads a speaker- or music-volume request. A number introduced by
// "to"/"at" is an absolute level; a number after a direction word without such
// a marker (e.g. "down by 30", "10 percent louder") is relative; a bare
// direction is one step of the caller's configured size. Mute/unmute are not
// volume changes and are left to the caller.
//
// The returned values mirror what the audio and Spotify Tools accept: a non-nil
// percent is an absolute level, a non-zero delta is a relative move, and ok is
// false when the text names no volume change at all (so the request can fall
// through to the Agent).
func VolumeChange(text string, step int) (percent *int, delta int, ok bool) {
	up, down := volumeUpRe.MatchString(text), volumeDownRe.MatchString(text)
	if n, nok := Number(text); nok {
		if n < 0 || n > 100 {
			return nil, 0, false
		}
		// "down by 20 percent" and "10 percent louder" are relative; "to 30"
		// (or no direction word) is an absolute level. The "to"/"at" marker is
		// tested against the text after the word so spoken numbers ("to
		// twenty") count as an absolute target too.
		if (up || down) && !toTargetLevel(text) {
			if n == 0 || (up && down) {
				return nil, 0, false
			}
			if down {
				n = -n
			}
			return nil, n, true
		}
		return &n, 0, true
	}
	switch {
	case down:
		return nil, -step, true
	case up:
		return nil, step, true
	}
	return nil, 0, false
}

// toTargetLevel reports whether text contains a "to"/"at" word followed by a
// number, marking the number as an absolute target level rather than a relative
// step.
func toTargetLevel(text string) bool {
	loc := volumeToRe.FindStringIndex(text)
	if loc == nil {
		return false
	}
	_, ok := Number(text[loc[1]:])
	return ok
}
