package calendar

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: TodayOut{
			Day: "today",
			Events: []Event{
				{
					Summary: "Team Sync",
					Start:   "2026-11-16T09:30:00Z",
					End:     "2026-11-16T10:00:00Z",
					AllDay:  false,
				},
			},
			Spoken: "You have 1 event today: Team Sync at 9:30.",
		},
		NextName: NextOut{
			Event: &Event{
				Summary: "Team Sync",
				Start:   "2026-11-16T09:30:00Z",
				End:     "2026-11-16T10:00:00Z",
				AllDay:  false,
			},
			Spoken: "Your next event is Team Sync at 9:30.",
		},
	}
}
