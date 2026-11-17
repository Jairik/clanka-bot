package assistant

import (
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
)

// ConformanceSamples gives the conformance suite one representative output per
// Speak Tool so the real renderers are exercised without a live reply.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		RepeatName: RepeatOut{
			Text: "It's 3:04 PM.",
			Tool: "time.now",
			Path: "fast_path",
			At:   time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC),
		},
		HeardName: HeardOut{
			Text:      `I heard: "what time is it".`,
			Utterance: "what time is it",
			At:        time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC),
			Source:    "voice",
		},
		HelpName: HelpOut{
			Tools: []HelpTool{{Name: "time.now", Description: "Get the current local time.", Permission: "read"}},
			Text:  "Here's what I can do: Get the current local time.",
		},
		ForgetName: ForgetOut{Text: "Forgotten."},
		AnnouncementsName: AnnouncementsOut{
			Lines: []announce.Line{
				{
					Text:       "Dinner is ready.",
					EnqueuedAt: time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC),
					Kind:       announce.KindGeneric,
					State:      announce.StateSpoken,
				},
			},
			Text: "Ten minutes ago: Dinner is ready.",
		},
	}
}
