package audiocontrol

// ConformanceSamples gives the conformance suite one representative output per
// Tool so the real Speak renderers are exercised.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		VolumeName: VolumeOut{Percent: 40},
		StopName:   StopOut{Stopped: true},
	}
}
