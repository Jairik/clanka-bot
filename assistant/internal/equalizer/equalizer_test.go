// Controller tests exercise routing and the audible fallback without a sound device.
package equalizer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, raw := range []string{`{}`, `{"Preset":"No","Bands":[0,0,0,0,0,0,0,0,0,0]}`, `{"Preset":"Custom","Bands":[13,0,0,0,0,0,0,0,0,0]}`, `{"Preset":"Custom","Bands":[0.5,0,0,0,0,0,0,0,0,0]}`} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	p, err := Parse(Encode(Profile{Preset: "Bass Boost"}))
	if err != nil || p.Bands != Presets["Bass Boost"] {
		t.Fatal(p, err)
	}
}
func TestRoutingAndFallback(t *testing.T) {
	c := New("aec_music_sink")
	var calls []string
	fail := false
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case call == "list short modules":
			return "", nil
		case args[0] == "load-module":
			if fail {
				return "", errors.New("plugin missing")
			}
			return "12", nil
		case call == "--format=json list sinks":
			return `[{"index":1,"name":"aec_music_sink"},{"index":2,"name":"clanka_eq_a"}]`, nil
		case call == "--format=json list sink-inputs":
			return `[{"index":8,"sink":1,"owner_module":null},{"index":9,"sink":1,"owner_module":"12"}]`, nil
		}
		return "", nil
	}
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	joined := strings.Join(calls, "\n")
	if c.err != nil || c.Target() != "clanka_eq_a" || !strings.Contains(joined, "move-sink-input 8 clanka_eq_a") || strings.Contains(joined, "move-sink-input 9") {
		t.Fatal(c.err, joined)
	}
	if !strings.Contains(joined, "sink_master=aec_music_sink plugin=caps label=Eq10X2") {
		t.Fatal(joined)
	}
	fail = true
	c.sync(context.Background(), Encode(Profile{Preset: "Bass Boost"}))
	if c.Target() != "aec_music_sink" || c.Health(context.Background()) == nil {
		t.Fatal("missing flat fallback")
	}
	if !strings.Contains(strings.Join(calls, "\n"), "unload-module 12") {
		t.Fatal("old EQ still active")
	}
}

// A restarted assistant recovers the original AEC master rather than chaining filters.
func TestRecoverMaster(t *testing.T) {
	c := New("")
	var calls []string
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "list short modules":
			return "7\tmodule-ladspa-sink\tsink_name=clanka_eq_a sink_master=aec_music_sink plugin=caps\t0", nil
		case "--format=json list sinks", "--format=json list sink-inputs":
			return `[]`, nil
		}
		if args[0] == "load-module" {
			return "8", nil
		}
		return "", nil
	}
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	if c.master != "aec_music_sink" || c.Target() != "clanka_eq_b" || c.err != nil {
		t.Fatal(c.master, c.Target(), c.err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "unload-module 7") {
		t.Fatal(calls)
	}
}

// A host without the CAPS library says only "No such entity"; Health must name
// the package instead of leaving the operator with the raw pactl error.
func TestMissingPluginHint(t *testing.T) {
	c := New("bluez_output")
	c.run = func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "list short modules", "--format=json list sinks", "--format=json list sink-inputs":
			return `[]`, nil
		case "get-default-sink":
			return "bluez_output", nil
		}
		if args[0] == "load-module" {
			return "", errors.New("pactl load-module: exit status 1: Failure: No such entity")
		}
		return "", nil
	}
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	if c.err == nil || !strings.Contains(c.err.Error(), "install the caps package") || !strings.Contains(c.err.Error(), `sink "bluez_output" exists`) || !strings.Contains(c.err.Error(), "No such entity") {
		t.Fatal(c.err)
	}
}
