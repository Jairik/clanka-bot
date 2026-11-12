package alarm

import "time"

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	next := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	return map[string]any{
		SetName: SetOut{
			Alarm: Alarm{
				ID:          "1",
				Hour:        7,
				Schedule:    Daily,
				NextFiresAt: next,
			},
		},
		ListName: ListOut{Alarms: []Alarm{{
			ID:          "1",
			Label:       "work",
			Hour:        7,
			Schedule:    Weekdays,
			NextFiresAt: next,
		}}},
		CancelName: CancelOut{Cancelled: 1},
		UpdateName: UpdateOut{
			Alarm: Alarm{
				ID:          "1",
				Hour:        7,
				Schedule:    Daily,
				NextFiresAt: next,
				Sound:       "birds",
			},
			Matched: 1,
			Changed: []string{"sound"},
		},
	}
}
