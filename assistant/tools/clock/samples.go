package clock

import "time"

// ConformanceSamples gives the conformance suite one representative output per
// Tool so the real Speak renderers are exercised.
func (*Module) ConformanceSamples() map[string]any {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		// The city table's zone is shipped and tested; a missing zone must not
		// panic the conformance sample if a stripped build omits tzdata.
		tokyo = time.UTC
	}
	return map[string]any{
		Name:          Out{Time: at},
		DateName:      DateOut{Date: at},
		CityName:      CityOut{City: "Tokyo", Timezone: "Asia/Tokyo", Time: at.In(tokyo)},
		CountdownName: CountdownOut{Event: "Christmas", Days: 44, Date: "2026-12-25"},
	}
}
