package lights

// ConformanceSamples covers all four spoken acknowledgements and the saved
// light listing.
func (*Module) ConformanceSamples() map[string]any {
	out := map[string]any{}
	for _, op := range []string{"power", "brightness", "color", "mode"} {
		out["lights."+op] = Out{Reply: "Light command sent."}
	}
	out["lights.list"] = ListOut{Devices: []ListDevice{{Name: "Desk Lamp", Protocol: "lotus"}}}
	return out
}
