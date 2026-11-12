package persona

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when an Out type changes
// (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		SwitchName: SwitchOut{Persona: "GLaDOS", Voice: "alba"},
		ListName: ListOut{
			Personas: []ListItem{
				{Name: "GLaDOS", Voice: "alba", Active: true},
				{Name: "Alfred", Voice: "alba"},
			},
			Active: "GLaDOS",
		},
		CurrentName: CurrentOut{
			Persona:              "GLaDOS",
			Voice:                "alba",
			EffectiveVoice:       "alba",
			EffectiveFiller:      "One moment.",
			EffectiveUnavailable: "Sorry, I can't answer that right now.",
			Filler:               "One moment.",
			Unavailable:          "Sorry, I can't answer that right now.",
			Exact:                true,
		},
	}
}
