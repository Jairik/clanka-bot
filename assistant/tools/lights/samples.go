package lights

// ConformanceSamples covers all four spoken acknowledgements.
func (*Module) ConformanceSamples() map[string]any {
	out := map[string]any{}
	for _, op := range []string{"power", "brightness", "color", "mode"} {
		out["lights."+op] = Out{Reply: "Light command sent."}
	}
	return out
}
