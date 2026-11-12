package calc

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: Out{Expression: "15% of 80", Value: 12, Display: "12"},
	}
}
