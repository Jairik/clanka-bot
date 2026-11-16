package weather

// ConformanceSamples gives the conformance suite one representative output for
// each Speak Tool so the real Speak renderer is exercised.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		Name: Out{
			City:         "Berlin",
			Condition:    "Partly cloudy",
			TemperatureC: 8.4,
			ApparentC:    6.1,
			WindKph:      9.2,
			Humidity:     70,
		},
		ForecastName: ForecastOut{
			City:       "Berlin",
			Day:        "tomorrow",
			Condition:  "light rain",
			HighC:      14,
			LowC:       6,
			RainChance: 70,
			Sunrise:    "06:42",
			Sunset:     "16:31",
		},
	}
}
