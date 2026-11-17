package reply

import (
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
)

// ignoreGate holds back the first few characters of a streamed reply until the
// Agent's ignore token can be ruled out, so a leading "<ignore>" cannot reach
// TTS.
// Waiting for the whole stream would delay every spoken answer, so the gate
// decides as soon as the token is complete or its prefix is broken (#498).
//
// A stream starting with the token is an ignore for the whole turn, matching
// agent.IsIgnoreReply: the model was told to send the token alone, so any text
// after it belongs to the same decision and is never spoken.
type ignoreGate struct {
	held    strings.Builder
	decided bool
	ignored bool
}

// Ignored reports whether the stream turned out to be the ignore signal.
func (g *ignoreGate) Ignored() bool { return g.ignored }

// Write consumes one streamed delta and returns the text that is safe to speak
// now. It returns "" while the start is still ambiguous or after an ignore.
func (g *ignoreGate) Write(delta string) string {
	if g.ignored || delta == "" {
		return ""
	}
	if g.decided {
		return delta
	}
	g.held.WriteString(delta)
	return g.resolve(false)
}

// Flush ends the stream and returns any held text. After an ignore it returns
// nothing: the caller speaks no words for that turn.
func (g *ignoreGate) Flush() string {
	if g.ignored || g.decided {
		return ""
	}
	return g.resolve(true)
}

// resolve applies the decision rules to the held text. final says no more
// deltas are coming, so a held token or whitespace must now resolve.
func (g *ignoreGate) resolve(final bool) string {
	if g.ignored || g.decided {
		return ""
	}
	held := g.held.String()
	candidate := strings.TrimLeft(held, " \t\r\n")
	if !final && (candidate == "" || isIgnorePrefix(candidate)) {
		// Still whitespace, or still a strict prefix of the token: hold.
		return ""
	}
	if agent.IsIgnoreReply(candidate) {
		g.ignored = true
		g.held.Reset()
		return ""
	}
	g.decided = true
	g.held.Reset()
	return held
}

// isIgnorePrefix reports whether candidate is the start of the ignore token, so
// one more delta could still complete it.
func isIgnorePrefix(candidate string) bool {
	token := agent.IgnoreToken
	if len(candidate) > len(token) {
		return false
	}
	return strings.EqualFold(candidate, token[:len(candidate)])
}
