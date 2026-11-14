// Package equalizer owns the global Pulse LADSPA stage ahead of the AEC sink.
package equalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

const Key = "audio.equalizer"
const CustomKey = "audio.equalizer_custom"

// Profile is stored atomically so a restart never restores half an edit.
type Profile struct {
	Preset string
	Bands  [10]int
}

var Presets = map[string][10]int{
	"Flat": {}, "Bass Boost": {6, 5, 4, 2, 0, 0, 0, 0, 0, 0},
	"Vocal Clarity": {-3, -2, -1, 0, 2, 3, 4, 3, 1, 0}, "Treble Boost": {0, 0, 0, 0, 0, 0, 2, 4, 5, 6},
}

// Parse rejects unknown presets, malformed arrays and out-of-range gains.
func Parse(raw string) (Profile, error) {
	var wire struct {
		Preset string
		Bands  []int
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return Profile{}, err
	}
	if len(wire.Bands) != 10 {
		return Profile{}, fmt.Errorf("exactly ten bands required")
	}
	p := Profile{Preset: wire.Preset}
	copy(p.Bands[:], wire.Bands)
	for _, gain := range p.Bands {
		if gain < -12 || gain > 12 {
			return p, fmt.Errorf("gain must be -12 to 12 dB")
		}
	}
	if gains, ok := Presets[p.Preset]; ok {
		p.Bands = gains
	} else if p.Preset != "Custom" {
		return p, fmt.Errorf("unknown preset")
	}
	return p, nil
}
func Encode(p Profile) string { b, _ := json.Marshal(p); return string(b) }
func Definitions() []settings.Definition {
	validate := settings.WithValidator(func(v any) error { _, err := Parse(v.(string)); return err })
	return []settings.Definition{settings.String(Key, Encode(Profile{Preset: "Flat"}), validate), settings.String(CustomKey, Encode(Profile{Preset: "Custom"}), validate)}
}

// Controller swaps filters while keeping the original sink as the flat fallback.
type Controller struct {
	mu                            sync.Mutex
	run                           func(context.Context, ...string) (string, error)
	master, name, module, applied string
	err                           error
}

func New(master string) *Controller {
	return &Controller{master: master, run: func(ctx context.Context, args ...string) (string, error) {
		b, e := exec.CommandContext(ctx, "pactl", args...).CombinedOutput()
		if e != nil {
			return "", fmt.Errorf("pactl %s: %w: %s", args[0], e, b)
		}
		return strings.TrimSpace(string(b)), nil
	}}
}

// loadHint names the fixes for a failed filter load. PulseAudio and
// PipeWire-Pulse answer both a missing LADSPA plugin and a missing master sink
// with "No such entity", which never says what an operator has to change.
func (c *Controller) loadHint(err error) error {
	for _, marker := range []string{"No such entity", "No such file", "not found"} {
		if strings.Contains(err.Error(), marker) {
			return fmt.Errorf("%w (install the caps package on the audio host and confirm sink %q exists)", err, c.master)
		}
	}
	return err
}

// Target lets newly opened speech and Cue streams enter the current filter immediately.
func (c *Controller) Target() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.name != "" {
		return c.name
	}
	return c.master
}

// Master keeps speaker volume on the original output when EQ becomes the default.
func (c *Controller) Master() string               { c.mu.Lock(); defer c.mu.Unlock(); return c.master }
func (c *Controller) Health(context.Context) error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// Watch also restores the desired filter after the host audio server restarts.
func (c *Controller) Watch(ctx context.Context, s *settings.Service) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		c.sync(ctx, s.String(Key))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (c *Controller) sync(parent context.Context, raw string) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applied == "" && c.name == "" {
		// Recover only our named filters, including their original master after an assistant restart.
		out, err := c.run(ctx, "list", "short", "modules")
		if err != nil {
			c.err = err
			return
		}
		// PulseAudio 16 omits module indices in JSON; the short format retains them.
		for _, line := range strings.Split(out, "\n") {
			columns := strings.Split(line, "\t")
			if len(columns) < 3 || columns[1] != "module-ladspa-sink" {
				continue
			}
			moduleID, err := strconv.Atoi(columns[0])
			if err != nil {
				continue
			}
			fields := strings.Fields(columns[2])
			name, master := "", ""
			for _, field := range fields {
				if strings.HasPrefix(field, "sink_name=") {
					name = strings.TrimPrefix(field, "sink_name=")
				}
				if strings.HasPrefix(field, "sink_master=") {
					master = strings.TrimPrefix(field, "sink_master=")
				}
			}
			if (name == "clanka_eq_a" || name == "clanka_eq_b") && master != "" {
				c.name = name
				c.module = strconv.Itoa(moduleID)
				if c.master == "" {
					c.master = master
				}
				break
			}
		}
	}
	if c.master == "" {
		var err error
		c.master, err = c.run(ctx, "get-default-sink")
		if err != nil {
			c.err = err
			return
		}
	}
	// A disappearing filter means Pulse restarted; never retain a stale module id.
	if c.name != "" {
		if _, err := c.run(ctx, "get-sink-volume", c.name); err != nil {
			c.name = ""
			c.module = ""
			c.applied = ""
		}
	}
	if raw != c.applied {
		p, err := Parse(raw)
		if err != nil {
			c.err = err
			return
		}
		name := "clanka_eq_a"
		if c.name == name {
			name = "clanka_eq_b"
		}
		controls := make([]string, 10)
		for i, g := range p.Bands {
			controls[i] = strconv.Itoa(g)
		}
		id, err := c.run(ctx, "load-module", "module-ladspa-sink", "sink_name="+name, "sink_master="+c.master, "plugin=caps", "label=Eq10X2", "channels=2", "rate=48000", "control="+strings.Join(controls, ","))
		if err != nil {
			c.err = fmt.Errorf("EQ unavailable; flat audio fallback: %w", c.loadHint(err))
			c.fallback(ctx)
			return
		}
		oldName, oldModule := c.name, c.module
		c.name, c.module, c.applied = name, id, raw
		if err = c.route(ctx, oldName); err != nil {
			c.err = err
			c.fallback(ctx)
			return
		}
		if oldModule != "" {
			if _, err = c.run(ctx, "unload-module", oldModule); err != nil {
				c.err = err
				return
			}
		}
	}
	c.err = c.route(ctx, "")
}

// route includes already-playing music and clients pinned to the configured sink.
func (c *Controller) route(ctx context.Context, old string) error {
	if _, err := c.run(ctx, "set-default-sink", c.name); err != nil {
		return err
	}
	out, err := c.run(ctx, "--format=json", "list", "sinks")
	if err != nil {
		return err
	}
	var sinks []struct {
		Index int
		Name  string
	}
	if err = json.Unmarshal([]byte(out), &sinks); err != nil {
		return err
	}
	targets := map[int]bool{}
	for _, s := range sinks {
		if s.Name == c.master || s.Name == old {
			targets[s.Index] = true
		}
	}
	out, err = c.run(ctx, "--format=json", "list", "sink-inputs")
	if err != nil {
		return err
	}
	var inputs []struct {
		Index       int
		Driver      string
		Properties  map[string]string
		Sink        int
		OwnerModule json.RawMessage `json:"owner_module"`
	}
	if err = json.Unmarshal([]byte(out), &inputs); err != nil {
		return err
	}
	for _, in := range inputs {
		// Filter outputs must stay on the master, or the graph would feed back.
		if targets[in.Sink] && !strings.Contains(in.Driver, "ladspa") && in.Properties["media.role"] != "filter" && strings.Trim(string(in.OwnerModule), `"`) != c.module {
			if _, err = c.run(ctx, "move-sink-input", strconv.Itoa(in.Index), c.name); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *Controller) fallback(_ context.Context) {
	// Cleanup gets its own budget even if the failed operation exhausted its deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = c.run(ctx, "set-default-sink", c.master)
	if c.module != "" {
		_, _ = c.run(ctx, "unload-module", c.module)
	}
	c.name, c.module, c.applied = "", "", ""
}
