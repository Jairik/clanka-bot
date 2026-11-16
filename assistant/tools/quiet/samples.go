package quiet

import (
	quietcore "github.com/Jairik/clanka-bot/assistant/internal/quiet"
)

// ConformanceSamples gives the conformance suite one representative output for
// every Speak Tool; keep it in sync when an Out type changes.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		SetName:   SetOut{Start: "22:30", End: "07:00"},
		OffName:   OffOut{Off: true},
		DNDName:   DNDOut{DurationSeconds: 7200, Until: "2026-11-12T01:00:00Z"},
		AwakeName: AwakeOut{Bypassed: true},
		StatusName: StatusOut{
			Enabled:  true,
			Start:    "22:30",
			End:      "07:00",
			Volume:   35,
			InWindow: true,
			Reason:   string(quietcore.ReasonWindow),
		},
	}
}
