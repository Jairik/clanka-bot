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
//
// The lock guards only the small state record; every pactl subprocess runs
// outside it so a playback start (which reads Target under the lock) never waits
// for a whole reconciliation pass. Only Watch's single ticker drives sync, so no
// second sync races the one in flight.
type Controller struct {
	mu                            sync.Mutex
	run                           func(context.Context, ...string) (string, error)
	configured                    bool
	master, name, module, applied string
	// cachedTargets caches the sink indices that must stay on the master (and
	// the filter just replaced) so the steady-state tick can re-run the input
	// moves without re-listing the sinks.
	cachedTargets                 map[int]bool
	err                           error
}

func New(master string) *Controller {
	return &Controller{
		configured: master != "",
		master:     master,
		run: func(ctx context.Context, args ...string) (string, error) {
			b, e := exec.CommandContext(ctx, "pactl", args...).CombinedOutput()
			if e != nil {
				return "", fmt.Errorf("pactl %s: %w: %s", args[0], e, b)
			}
			return strings.TrimSpace(string(b)), nil
		},
	}
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

// sync snapshots the state under a short lock, runs every pactl call outside it,
// then re-acquires the lock to publish the result. A settings change that lands
// in the middle of the I/O is detected at publish time: if the live applied
// value no longer matches the snapshot, the new profile is left unapplied so the
// next tick re-runs instead of committing a stale result.
func (c *Controller) sync(parent context.Context, raw string) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()

	c.mu.Lock()
	snap := struct {
		configured bool
		master     string
		name       string
		module     string
		applied    string
		targets    map[int]bool
	}{c.configured, c.master, c.name, c.module, c.applied, c.cachedTargets}
	c.mu.Unlock()

	configured, master, name, module, applied, targets := snap.configured, snap.master, snap.name, snap.module, snap.applied, snap.targets
	dirty, loaded := false, false
	var syncErr error

	// 1. Recover one of our named filters from a prior run (including its
	//    original master) after an assistant restart. Only when nothing is
	//    applied and no filter is active yet.
	if applied == "" && name == "" {
		rName, rModule, rMaster, err := c.recoverModules(ctx)
		if err != nil {
			syncErr = err
		} else if rName != "" {
			name, module, dirty = rName, rModule, true
			if master == "" {
				master = rMaster
			}
		}
	}

	// 2. Learn the master from the host default when none was configured or
	//    recovered.
	if syncErr == nil && master == "" {
		got, err := c.run(ctx, "get-default-sink")
		if err != nil {
			syncErr = err
		} else {
			master = got
			dirty = true
		}
	}

	// 3. A disappearing filter means Pulse restarted; drop the active name so
	//    the profile is reloaded, but keep the module id so the stale filter is
	//    cleaned up after the reload. This cheap per-tick check keeps the heavier
	//    sink listing out of the steady state.
	if syncErr == nil && name != "" {
		if _, err := c.run(ctx, "get-sink-volume", name); err != nil {
			name, applied, dirty = "", "", true
		}
	}

	// 4. Apply the profile when it changed (or no filter is active yet). All
	//    pactl calls here run outside the lock.
	if syncErr == nil && raw != applied {
		newName, newModule, newMaster, newTargets, err := c.loadProfile(ctx, raw, master, name, configured)
		if err != nil {
			// Flat fallback: point the default back at the master and drop the
			// stale filter, then leave the controller flat.
			c.fallback(ctx, master, module)
			name, module, applied = "", "", ""
			syncErr = err
		} else {
			oldModule := module
			name, module, master, applied, targets = newName, newModule, newMaster, raw, newTargets
			loaded, dirty = true, true
			if oldModule != "" && oldModule != newModule {
				if _, err := c.run(ctx, "unload-module", oldModule); err != nil {
					syncErr = err
				}
			}
		}
	}

	// 5. Each tick re-routes the current inputs onto the active filter. After a
	//    load or recovery the route already ran; in steady state we only list
	//    the inputs and move them using the cached sink list, never
	//    set-default-sink or a fresh sink listing.
	if syncErr == nil && name != "" {
		if loaded {
			// routing already performed by loadProfile
		} else if dirty || targets == nil {
			targets, syncErr = c.route(ctx, name, master, module, "")
		} else {
			syncErr = c.moveInputs(ctx, name, module, targets)
		}
	}

	// Publish under the lock. If the live applied value changed during the I/O
	// (a settings edit or a concurrent sync), do not overwrite it; leave the
	// next tick to re-run.
	c.mu.Lock()
	if syncErr == nil && c.applied == snap.applied {
		c.applied = applied
	}
	c.master, c.name, c.module, c.cachedTargets, c.err = master, name, module, targets, syncErr
	c.mu.Unlock()
}

// recoverModules finds a filter we previously loaded so a restart does not chain
// filters. It returns the recovered filter name, its module id and the master it
// was built on.
func (c *Controller) recoverModules(ctx context.Context) (string, string, string, error) {
	out, err := c.run(ctx, "list", "short", "modules")
	if err != nil {
		return "", "", "", err
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
			return name, strconv.Itoa(moduleID), master, nil
		}
	}
	return "", "", "", nil
}

// loadProfile loads the desired profile onto the next named filter and routes
// the inputs onto it. For a non-configured master (learned from the host
// default, or recovered) with no healthy filter loaded, a "No such entity"
// failure re-reads the host default once and retries with the new master. The
// returned master reflects any re-resolution so the caller can publish it.
func (c *Controller) loadProfile(ctx context.Context, raw, master, name string, configured bool) (newName, newModule, newMaster string, targets map[int]bool, err error) {
	p, err := Parse(raw)
	if err != nil {
		return "", "", master, nil, err
	}
	newName = "clanka_eq_a"
	if name == newName {
		newName = "clanka_eq_b"
	}
	controls := make([]string, 10)
	for i, g := range p.Bands {
		controls[i] = strconv.Itoa(g)
	}
	args := []string{
		"load-module", "module-ladspa-sink",
		"sink_name=" + newName, "sink_master=" + master,
		"plugin=caps", "label=Eq10X2", "channels=2", "rate=48000",
		"control=" + strings.Join(controls, ","),
	}
	id, loadErr := c.run(ctx, args...)
	if loadErr != nil {
		// Non-configured master and no healthy filter: re-resolve the host
		// default once inside the same 3 s context and retry. Never re-resolve
		// while a healthy filter is loaded.
		if !configured && name == "" {
			if resolved, reErr := c.run(ctx, "get-default-sink"); reErr == nil {
				master = resolved
				args[3] = "sink_master=" + master
				id, loadErr = c.run(ctx, args...)
			}
		}
		if loadErr != nil {
			wrapped := c.loadHint(loadErr)
			if !configured {
				wrapped = fmt.Errorf("%w (the host's default sink may have changed; the assistant will re-resolve it)", wrapped)
			}
			return "", "", master, nil, wrapped
		}
	}
	targets, err = c.route(ctx, newName, master, id, name)
	if err != nil {
		return "", "", master, nil, err
	}
	return newName, id, master, targets, nil
}

// route makes the filter the default sink, lists the sinks to map their indices,
// lists the inputs and moves every input that is not already on the master or
// the filter being replaced. It returns the sink indices that must stay on the
// master (and the replaced filter) so the steady-state tick can re-run the moves
// without re-listing sinks.
func (c *Controller) route(ctx context.Context, name, master, module, old string) (map[int]bool, error) {
	if _, err := c.run(ctx, "set-default-sink", name); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "--format=json", "list", "sinks")
	if err != nil {
		return nil, err
	}
	var sinks []struct {
		Index int
		Name  string
	}
	if err = json.Unmarshal([]byte(out), &sinks); err != nil {
		return nil, err
	}
	targets := map[int]bool{}
	for _, s := range sinks {
		if s.Name == master || s.Name == old {
			targets[s.Index] = true
		}
	}
	inputs, err := c.listInputs(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.applyMoves(ctx, name, module, targets, inputs); err != nil {
		return nil, err
	}
	return targets, nil
}

// moveInputs re-lists the current inputs and moves them using a cached sink map.
func (c *Controller) moveInputs(ctx context.Context, name, module string, targets map[int]bool) error {
	inputs, err := c.listInputs(ctx)
	if err != nil {
		return err
	}
	return c.applyMoves(ctx, name, module, targets, inputs)
}

func (c *Controller) listInputs(ctx context.Context) ([]sinkInput, error) {
	out, err := c.run(ctx, "--format=json", "list", "sink-inputs")
	if err != nil {
		return nil, err
	}
	var inputs []sinkInput
	if err = json.Unmarshal([]byte(out), &inputs); err != nil {
		return nil, err
	}
	return inputs, nil
}

type sinkInput struct {
	Index       int
	Driver      string
	Properties  map[string]string
	Sink        int
	OwnerModule json.RawMessage `json:"owner_module"`
}

// applyMoves moves every input that is on the master (or the filter being
// replaced) and is not the filter's own stream onto the active filter.
func (c *Controller) applyMoves(ctx context.Context, name, module string, targets map[int]bool, inputs []sinkInput) error {
	for _, in := range inputs {
		// Filter outputs must stay on the master, or the graph would feed back.
		if targets[in.Sink] && !strings.Contains(in.Driver, "ladspa") && in.Properties["media.role"] != "filter" && strings.Trim(string(in.OwnerModule), `"`) != module {
			if _, err := c.run(ctx, "move-sink-input", strconv.Itoa(in.Index), name); err != nil {
				return err
			}
		}
	}
	return nil
}

// fallback points the default sink back at the master and drops a stale filter.
// It runs its own budget even if the failed operation exhausted its deadline,
// and it runs outside the controller lock so a playback start is never blocked.
func (c *Controller) fallback(_ context.Context, master, module string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = c.run(ctx, "set-default-sink", master)
	if module != "" {
		_, _ = c.run(ctx, "unload-module", module)
	}
}
