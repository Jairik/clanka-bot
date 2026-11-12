package timer

// ConformanceSamples gives the conformance suite one representative output per
// Tool so the real Speak renderers are exercised.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		SetName:    Timer{DurationSeconds: 300, RemainingSeconds: 300},
		CancelName: CancelOut{Cancelled: 1},
		ListName:   ListOut{Timers: []Timer{{DurationSeconds: 300, RemainingSeconds: 120}}},
	}
}
