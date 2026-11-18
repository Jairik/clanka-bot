package conversation

// ConformanceSamples gives the conformance suite one representative output per
// Speak Tool so the real renderers are exercised without a live latch.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		StartName: StartOut{Text: "Let's talk."},
		EndName:   EndOut{Text: "Okay."},
	}
}
