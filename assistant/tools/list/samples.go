package list

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes (docs/agents/adding-a-tool.md).
func (*Module) ConformanceSamples() map[string]any {
	item := Item{ID: "1", List: KeyShopping, Text: "milk"}
	return map[string]any{
		AddName:    AddOut{Item: item, Added: true},
		ReadName:   ReadOut{Lists: []List{{Key: KeyShopping, Title: Title(KeyShopping), Items: []Item{item}}}},
		RemoveName: RemoveOut{Removed: true, Item: item},
	}
}
