package reminder

// ConformanceSamples gives the conformance suite one successful output per
// Tool so the real Speak renderers are exercised without scheduling anything.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		SetName: Reminder{
			ID: "1", Message: "check the oven", Kind: KindDuration,
			DurationSeconds: 1200, RemainingSeconds: 1200, When: "in 20 minutes",
		},
		ListName: ListOut{Reminders: []Reminder{{
			ID: "1", Message: "call mum", Kind: KindClock,
			RemainingSeconds: 3600, When: "at 5:00 pm",
		}}},
		CancelName: CancelOut{Cancelled: 1},
	}
}
