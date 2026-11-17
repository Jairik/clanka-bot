package agent

import (
	"fmt"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Live setting keys for the Agent path's length and cost bounds. They are read
// once per turn, which is also the seam conversation mode uses to lift them for
// one request.
const (
	SpokenMaxSentencesKey  = "agent.spoken_max_sentences"
	SpokenMaxCharsKey      = "agent.spoken_max_chars"
	MaxCompletionTokensKey = "agent.max_completion_tokens"
)

// Code defaults for Limits.
const (
	DefaultSpokenMaxSentences  = 2
	DefaultSpokenMaxChars      = 220
	DefaultMaxCompletionTokens = 300
)

// Limits are the per-turn bounds on one Agent answer: how much may be spoken
// and how many completion tokens the provider may generate.
type Limits struct {
	SpokenMaxSentences  int
	SpokenMaxChars      int
	MaxCompletionTokens int
}

// DefaultLimits is the brevity budget the Agent path ships with.
func DefaultLimits() Limits {
	return Limits{
		SpokenMaxSentences:  DefaultSpokenMaxSentences,
		SpokenMaxChars:      DefaultSpokenMaxChars,
		MaxCompletionTokens: DefaultMaxCompletionTokens,
	}
}

// LimitsDefinitions declares the three live settings. A conversation turn can
// read or override the same keys; the provider reads them before every request.
func LimitsDefinitions() []settings.Definition {
	return []settings.Definition{
		settings.Int(SpokenMaxSentencesKey, DefaultSpokenMaxSentences,
			settings.WithDescription("Most sentences the Agent may speak per reply. The stream is cancelled at the next sentence boundary."),
			settings.WithValidator(func(value any) error {
				n := value.(int)
				if n < 1 || n > 20 {
					return fmt.Errorf("spoken sentence budget must be 1–20")
				}
				return nil
			})),
		settings.Int(SpokenMaxCharsKey, DefaultSpokenMaxChars,
			settings.WithDescription("Approximate character budget for an Agent reply. The stream is cancelled at the next sentence boundary after it is reached."),
			settings.WithValidator(func(value any) error {
				n := value.(int)
				if n < 40 || n > 4000 {
					return fmt.Errorf("spoken character budget must be 40–4000")
				}
				return nil
			})),
		settings.Int(MaxCompletionTokensKey, DefaultMaxCompletionTokens,
			settings.WithDescription("Maximum completion tokens the Agent may generate per model step. A cost backstop, not the spoken length limit."),
			settings.WithValidator(func(value any) error {
				n := value.(int)
				if n < 1 || n > 8000 {
					return fmt.Errorf("completion token cap must be 1–8000")
				}
				return nil
			})),
	}
}

// resolveLimits applies the code defaults to a zero Limits, then overlays the
// live per-turn values. A zero live field keeps the static one.
func resolveLimits(base Limits, live func() Limits) Limits {
	out := base
	defaults := DefaultLimits()
	if out.SpokenMaxSentences <= 0 {
		out.SpokenMaxSentences = defaults.SpokenMaxSentences
	}
	if out.SpokenMaxChars <= 0 {
		out.SpokenMaxChars = defaults.SpokenMaxChars
	}
	if out.MaxCompletionTokens <= 0 {
		out.MaxCompletionTokens = defaults.MaxCompletionTokens
	}
	if live != nil {
		current := live()
		if current.SpokenMaxSentences > 0 {
			out.SpokenMaxSentences = current.SpokenMaxSentences
		}
		if current.SpokenMaxChars > 0 {
			out.SpokenMaxChars = current.SpokenMaxChars
		}
		if current.MaxCompletionTokens > 0 {
			out.MaxCompletionTokens = current.MaxCompletionTokens
		}
	}
	return out
}
