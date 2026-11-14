package system

import "time"

// ConformanceSamples gives the conformance suite one representative output so
// the real Speak renderer is exercised without touching a service.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: Out{
			OK:        true,
			CheckedAt: time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC),
			Services:  []Check{{Name: "embed", State: "ok"}},
			Summary:   "I'm OK. Everything I can check is responding.",
		},
	}
}
