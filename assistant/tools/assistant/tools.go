package assistant

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

// RepeatIn carries an optional speech-rate direction. "say that again" has no
// direction and repeats at the setting; "say that again slower" steps down.
type RepeatIn struct {
	Rate string `json:"rate,omitempty"`
}

// Rate directions Extract recognises. An empty string keeps the setting.
const (
	RepeatSlower = "slower"
	RepeatFaster = "faster"
	RepeatHalf   = "half"
	RepeatDouble = "double"
)

// repeatStep is the fraction of the previous rate one "slower"/"faster" step
// changes. 20% keeps the change audible without overshooting the setting.
const repeatStep = 0.20

// RepeatOut is the previous completed reply with its provenance. Rate, when
// non-zero, is the per-utterance TTS multiplier to speak this replay at; it is
// not a change to the persistent voice.rate setting.
type RepeatOut struct {
	Text      string    `json:"text"`
	Tool      string    `json:"tool,omitempty"`
	Path      string    `json:"path,omitempty"`
	LocalOnly bool      `json:"local_only,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Rate      float64   `json:"rate,omitempty"`
}

// ForgetIn has no fields: "forget the conversation" clears whatever is stored,
// it does not take the current utterance as its input.
type ForgetIn struct{}

// ForgetOut is the one-line confirmation that the history was cleared.
type ForgetOut struct {
	Text string `json:"text"`
}

// NewForget builds assistant.forget. It resets the shared conversation history
// on the Pi, so it works with no provider reachable. It is deliberately
// Local-only and Fast-path eligible (PermissionAct, not AgentOnly).
//
// It clears only conversation turns, never the last-reply store: "say that
// again" must still work after a forget. It does not clear Scheduled alerts,
// notes or lists; this is conversation state only. A nil history is a clear
// failure rather than a panic, for callers with no shared history wired.
func NewForget(history tool.HistoryResetter) tool.Tool[ForgetIn, ForgetOut] {
	return tool.Tool[ForgetIn, ForgetOut]{
		Name:        ForgetName,
		Description: "Forget the current conversation and start with a clean history.",
		Permission:  tool.PermissionAct,
		// A forget must work even when the provider is unreachable, and its
		// confirmation never leaves the Pi. LocalOnlyYes + AgentOnly is
		// rejected at registration, so this stays Fast-path eligible.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, ForgetIn) (ForgetOut, error) {
			if history == nil {
				return ForgetOut{}, &tool.ToolError{Say: "I can't forget anything right now."}
			}
			history.Reset()
			return ForgetOut{Text: "Forgotten."}, nil
		},
		Speak: func(out ForgetOut) string { return out.Text },
		Examples: []string{
			"forget that",
			"forget the conversation",
			"clear our chat",
			"start over",
			"never mind that",
			"forget everything we've talked about",
			"let's start fresh",
			"clear the conversation history",
		},
	}
}

// SpeechRate implements the per-result rate hook the pipeline reads after a
// successful Execute. Zero means "use the setting".
func (o RepeatOut) SpeechRate() float64 { return o.Rate }

// HelpIn has no fields.
type HelpIn struct{}

// HelpTool describes one enabled Tool. The help result is Local-only, so it
// may name Local-only Tools without exposing them to the Agent path.
type HelpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Permission  string `json:"permission"`
	LocalOnly   bool   `json:"local_only,omitempty"`
}

// HelpOut is the generated capability list and the one-line spoken form.
type HelpOut struct {
	Tools []HelpTool `json:"tools"`
	Text  string     `json:"text"`
}

// NewRepeat builds assistant.repeat. It reads the shared last-reply store at
// call time; a nil store or an empty history is a clear failure, never a
// repeated echo of the request itself. The optional rateSource returns the
// live voice.rate setting, used as the base when the last reply had no
// override; the variadic form keeps older callers source-compatible.
func NewRepeat(store *lastreply.Store, rateSource ...func() float64) tool.Tool[RepeatIn, RepeatOut] {
	var current func() float64
	if len(rateSource) > 0 {
		current = rateSource[0]
	}
	currentRate := func() float64 {
		if current == nil {
			return 1.0
		}
		if rate := current(); rate > 0 {
			return rate
		}
		return 1.0
	}
	return tool.Tool[RepeatIn, RepeatOut]{
		Name:        RepeatName,
		Description: "Repeat the last reply the Assistant gave, optionally slower or faster.",
		Permission:  tool.PermissionRead,
		// The previous reply may be a Local-only result, and the Agent path
		// must never see it as Tool output. Declaring the repeat Tool itself
		// Local-only is the explicit, safe choice.
		LocalOnly: tool.LocalOnlyYes,
		// The repeated text keeps the flag it had when it was spoken. This is
		// the one declared per-result exception, sitting next to LocalOnly so
		// it is reviewed with the privacy decision; it reports only the text
		// repeat replays, never new text of its own.
		Provenance: func(out RepeatOut) (bool, bool) { return out.LocalOnly, true },
		Handler: func(_ context.Context, in RepeatIn) (RepeatOut, error) {
			reply, ok := store.Last()
			if !ok || strings.TrimSpace(reply.Text) == "" {
				return RepeatOut{}, &tool.ToolError{Say: "I haven't said anything yet."}
			}
			return RepeatOut{
				Text:      reply.Text,
				Tool:      reply.Tool,
				Path:      reply.Path,
				LocalOnly: reply.LocalOnly,
				At:        reply.At,
				Rate:      repeatRate(in.Rate, reply.SpeechRate, currentRate),
			}, nil
		},
		Speak: func(out RepeatOut) string { return out.Text },
		Examples: []string{
			"say that again",
			"what did you say",
			"repeat that",
			"can you repeat that",
			"say it again",
			"what was that",
			"I missed that",
			"come again",
			"say that again slower",
			"say that again faster",
			"again but slower",
			"again but faster",
			"a bit slower",
			"say it again more slowly",
			"repeat that faster please",
			"at half speed",
			"at double speed",
		},
		Extract: extractRepeatRate,
	}
}

// repeatRate computes the per-utterance multiplier for one replay. An empty
// direction keeps the setting (0). A step is 20% of the rate the previous reply
// actually used, falling back to the current setting when it had no override.
// "half"/"double" are absolute. The result is clamped to the shared TTS range
// and rounded so repeated steps do not accumulate float noise; the persistent
// voice.rate setting is never touched.
func repeatRate(direction string, previous float64, current func() float64) float64 {
	if direction == "" {
		return 0
	}
	base := previous
	if base <= 0 && current != nil {
		base = current()
	}
	if base <= 0 {
		base = 1.0
	}
	var rate float64
	switch direction {
	case RepeatHalf:
		rate = wyoming.MinRate
	case RepeatDouble:
		rate = wyoming.MaxRate
	case RepeatSlower:
		rate = base * (1 - repeatStep)
	case RepeatFaster:
		rate = base * (1 + repeatStep)
	default:
		return 0
	}
	if rate < wyoming.MinRate {
		rate = wyoming.MinRate
	}
	if rate > wyoming.MaxRate {
		rate = wyoming.MaxRate
	}
	return math.Round(rate*100) / 100
}

// extractRepeatRate reads a speech-rate direction from a repeat utterance. Any
// repeat utterance is a match (an absent direction keeps the setting), so the
// semantic Fast path can still route a plain "say that again". It deliberately
// does not match music/playback speed, which belongs to other Tools.
func extractRepeatRate(text string) (RepeatIn, bool) {
	lower := strings.ToLower(text)
	for _, other := range []string{"music", "song", "playlist", "volume", "fan", "video"} {
		if strings.Contains(lower, other) {
			return RepeatIn{}, false
		}
	}
	switch {
	case strings.Contains(lower, "half speed"), strings.Contains(lower, "half as fast"), strings.Contains(lower, "half the speed"):
		return RepeatIn{Rate: RepeatHalf}, true
	case strings.Contains(lower, "double speed"), strings.Contains(lower, "twice as fast"), strings.Contains(lower, "double the speed"):
		return RepeatIn{Rate: RepeatDouble}, true
	case strings.Contains(lower, "slower"), strings.Contains(lower, "more slowly"), strings.Contains(lower, "slowly"):
		return RepeatIn{Rate: RepeatSlower}, true
	case strings.Contains(lower, "faster"), strings.Contains(lower, "more quickly"), strings.Contains(lower, "quicker"):
		return RepeatIn{Rate: RepeatFaster}, true
	default:
		return RepeatIn{}, true
	}
}

// NewHelp builds assistant.help. The handler walks the registry every call and
// keeps only Tools whose effective Enabled flag is true, so a Dashboard
// disable applies to the next request and no feature list lives in code.
func NewHelp(registry *tool.Registry) tool.Tool[HelpIn, HelpOut] {
	return tool.Tool[HelpIn, HelpOut]{
		Name:        HelpName,
		Description: "List the Tools that are enabled right now.",
		Permission:  tool.PermissionRead,
		// The list would name Local-only Tools, which the Agent path and the
		// MCP surface must not learn. Keeping the whole result on the Pi is
		// simpler and stricter than filtering the list per caller.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, HelpIn) (HelpOut, error) {
			if registry == nil {
				return HelpOut{Text: "I don't have any tools enabled right now."}, nil
			}
			names := registry.Names()
			out := HelpOut{Tools: make([]HelpTool, 0, len(names))}
			descriptions := make([]string, 0, len(names))
			for _, name := range names {
				info, ok := registry.Info(name)
				if !ok || !info.Enabled {
					continue
				}
				out.Tools = append(out.Tools, HelpTool{
					Name:        info.Name,
					Description: info.Description,
					Permission:  string(info.Permission),
					LocalOnly:   info.LocalOnly,
				})
				if text := strings.TrimSpace(info.Description); text != "" {
					descriptions = append(descriptions, text)
				}
			}
			out.Text = helpText(descriptions)
			return out, nil
		},
		Speak: func(out HelpOut) string { return out.Text },
		Examples: []string{
			"what can you do",
			"what can you help with",
			"what are your capabilities",
			"what are you able to do",
			"list your tools",
			"what tools do you have",
			"help",
			"tell me what you can do",
		},
	}
}

// helpText joins the enabled Tools' own descriptions. It is deliberately
// generated from the registry, so a new module appears without editing this
// package.
func helpText(descriptions []string) string {
	if len(descriptions) == 0 {
		return "I don't have any tools enabled right now."
	}
	return "Here's what I can do: " + strings.Join(descriptions, " ")
}
