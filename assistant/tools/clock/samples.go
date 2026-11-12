package clock

import "time"

// ConformanceSamples gives the conformance suite one representative output per
// Tool so the real Speak renderers are exercised.
func (*Module) ConformanceSamples() map[string]any {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	return map[string]any{
		Name:     Out{Time: at},
		DateName: DateOut{Date: at},
	}
}
