// Run explicitly inside an isolated PulseAudio server, never against the owner's session.
package equalizer

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestNativePulse(t *testing.T) {
	if os.Getenv("CLANKA_ISOLATED_PULSE_TEST") != "1" {
		t.Skip("requires an isolated Pulse server with CAPS")
	}
	ctx := context.Background()
	c := New("aec_music_sink")
	if _, err := c.run(ctx, "load-module", "module-null-sink", "sink_name=aec_music_sink"); err != nil {
		t.Fatal(err)
	}
	c.sync(ctx, Encode(Profile{Preset: "Bass Boost"}))
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	// Recover the original master even though the server default now names our EQ.
	recovered := New("")
	recovered.sync(ctx, Encode(Profile{Preset: "Vocal Clarity"}))
	if err := recovered.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if recovered.master != "aec_music_sink" {
		t.Fatal(recovered.master)
	}
	recovered.sync(ctx, Encode(Profile{Preset: "Vocal Clarity"}))
	if err := recovered.Health(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := c.run(ctx, "--format=json", "list", "sinks")
	if err != nil {
		t.Fatal(err)
	}
	var sinks []struct {
		Index int
		Name  string
	}
	if err = json.Unmarshal([]byte(out), &sinks); err != nil {
		t.Fatal(err)
	}
	master := -1
	filters := 0
	for _, s := range sinks {
		if s.Name == "aec_music_sink" {
			master = s.Index
		}
		if s.Name == "clanka_eq_a" || s.Name == "clanka_eq_b" {
			filters++
		}
	}
	if filters != 1 {
		t.Fatalf("remaining EQ sinks: %s", out)
	}
	out, err = c.run(ctx, "--format=json", "list", "sink-inputs")
	if err != nil {
		t.Fatal(err)
	}
	var inputs []struct{ Sink int }
	if err = json.Unmarshal([]byte(out), &inputs); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].Sink != master {
		t.Fatalf("filter output was moved into a feedback loop: %s", out)
	}
	recovered.fallback(ctx)
}
