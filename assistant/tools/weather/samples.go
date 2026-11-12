package weather

// ConformanceSamples gives the conformance suite one representative output for
// weather.current so the real Speak renderer is exercised.
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
	}
}
