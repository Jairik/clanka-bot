package units

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when Out changes.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: Out{Value: 5, From: "mi", To: "km", Result: 8.04672, Display: "8.04672"},
	}
}
