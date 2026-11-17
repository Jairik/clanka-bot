package agent

import "strings"

// IgnoreToken is the exact reply that means "that speech was not for me". The
// default prompt asks for it when an utterance is not a clear request or
// question addressed to the Assistant; Run recognises it so the turn never
// enters history, and the reply session holds it back from TTS (#498).
const IgnoreToken = "<ignore>"

// IsIgnoreReply reports whether a model reply is the ignore signal. The reply
// is trimmed, and surrounding whitespace or case does not change the meaning.
// A reply that begins with the token also counts: the model was told to send
// the token alone, so trailing text is noise from the same decision and must
// not be spoken or remembered. The reply session uses the same rule while it
// holds back the start of the stream, so the two can never disagree about one
// answer.
func IsIgnoreReply(reply string) bool {
	trimmed := strings.TrimSpace(reply)
	if len(trimmed) < len(IgnoreToken) {
		return false
	}
	return strings.EqualFold(trimmed[:len(IgnoreToken)], IgnoreToken)
}
