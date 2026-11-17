package monster

// ConformanceSamples covers all five Monster light tool spoken acknowledgements.
func (*Module) ConformanceSamples() map[string]any {
	return map[string]any{
		"monster.power":      Out{Reply: "Done."},
		"monster.brightness": Out{Reply: "Done."},
		"monster.color":      Out{Reply: "Done."},
		"monster.white":      Out{Reply: "Done."},
		"monster.scene":      Out{Reply: "Done."},
	}
}
