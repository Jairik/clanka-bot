package notify

// ConformanceSamples gives the conformance suite one representative output for
// notify.phone; keep it in sync when Out changes.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: Out{Sent: true, Source: "last_reply"},
	}
}
