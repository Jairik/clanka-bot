package note

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		RememberName: Note{ID: "1", Text: "the spare key is under the pot"},
		RecallName: RecallOut{Query: "spare key", Notes: []Match{{
			Note: Note{ID: "1", Text: "the spare key is under the pot"}, Score: 0.82,
		}}},
		ForgetName: ForgetOut{Deleted: 1},
	}
}
