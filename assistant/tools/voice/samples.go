package voice

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when an Out type changes
// (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		SetName: SetOut{Voice: "alba", Builtin: true, Rate: 1.0},
		ListName: ListOut{
			Voices: []ListItem{
				{Name: "alba", Status: "builtin", Builtin: true},
				{Name: "jarvis", Status: "ready"},
			},
			Current: "alba",
			Rate:    1.0,
		},
		CurrentName: CurrentOut{Voice: "alba", Builtin: true, Status: "builtin", Rate: 1.0},
		RateName:    RateOut{Rate: 1.1, Delta: 1},
	}
}
