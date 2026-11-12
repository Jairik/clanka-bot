package timer

// ConformanceSamples gives the conformance suite one representative output per
// Tool so the real Speak renderers are exercised.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		SetName:       Timer{ID: "1", Label: "pasta", DurationSeconds: 600, RemainingSeconds: 600},
		CancelName:    CancelOut{Cancelled: 1},
		RemainingName: RemainingOut{Timer: Timer{ID: "1", Label: "pasta", DurationSeconds: 600, RemainingSeconds: 420}},
		ListName:      ListOut{Timers: []Timer{{ID: "1", DurationSeconds: 300, RemainingSeconds: 120}}},
	}
}
