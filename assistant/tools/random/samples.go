package random

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when an Out type changes.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		FlipName: FlipOut{Side: "heads"},
		RollName: RollOut{Count: 1, Sides: 20, Rolls: []int{17}, Total: 17},
		PickName: PickOut{Options: []string{"pizza", "tacos"}, Choice: "tacos", Index: 1},
	}
}
