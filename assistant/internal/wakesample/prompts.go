package wakesample

// Prompts are the varied deliveries the Dashboard cycles through. They follow
// the same intent as record.py's list: a detector that only knows one loudness
// and one distance fails exactly where the assistant is used. The list is
// short on purpose, because the same prompts repeat as the count grows.
var Prompts = []string{
	"normal voice, your usual distance from the mic",
	"normal voice, slightly further back",
	"quiet voice, close to the mic (late at night)",
	"loud voice, as if calling across a room",
	"fast, clipped delivery",
	"slow, drawn out",
	"across the room, raised voice",
	"while music is playing",
	"from the phone, held at arm's length",
	"normal voice again",
}

// NegativePrompts are near-misses and ordinary speech. The first few are the
// clanker-shaped near misses from training/near_misses.txt; the rest are
// ordinary requests that must not wake the assistant. Keeping them here means
// the page needs no training checkout at runtime.
var NegativePrompts = []string{
	"clank",
	"clunk",
	"clinker",
	"clan car",
	"hey clanker",
	"clanker please",
	"what time is it",
	"turn on the kitchen light",
	"play some music",
	"stop the music",
}

// PromptFor picks the next delivery prompt from the count already kept. The
// count is server-side, so a page reload does not restart the cycle.
func PromptFor(count int) string {
	if len(Prompts) == 0 {
		return ""
	}
	if count < 0 {
		count = 0
	}
	return Prompts[count%len(Prompts)]
}

// NegativePromptFor is the negative tab's counterpart.
func NegativePromptFor(count int) string {
	if len(NegativePrompts) == 0 {
		return ""
	}
	if count < 0 {
		count = 0
	}
	return NegativePrompts[count%len(NegativePrompts)]
}
