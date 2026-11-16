// Controller tests exercise routing and the audible fallback without a sound device.
package equalizer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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

// countContains counts the recorded calls that contain substr.
func countContains(calls []string, substr string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// syncRecords is a pactl seam that records every call and answers the routing
// shapes the tests need.
type syncRecords struct {
	mu    sync.Mutex
	calls []string
}

func (r *syncRecords) record(call string) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
}

func (r *syncRecords) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, "\n")
}

// TestSyncDoesNotHoldLockAcrossPactl proves Target() returns while sync is
// blocked inside a pactl subprocess, so a playback start never waits for a
// reconciliation pass. Run under -race to catch any shared-state access.
func TestSyncDoesNotHoldLockAcrossPactl(t *testing.T) {
	c := New("aec_music_sink")
	rec := &syncRecords{}
	started := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		rec.record(call)
		once.Do(func() { close(started); <-proceed })
		switch {
		case call == "list short modules":
			return "", nil
		case args[0] == "load-module":
			return "12", nil
		case call == "--format=json list sinks":
			return `[{"index":1,"name":"aec_music_sink"},{"index":2,"name":"clanka_eq_a"}]`, nil
		case call == "--format=json list sink-inputs":
			return `[{"index":8,"sink":1,"owner_module":null}]`, nil
		case call == "get-default-sink":
			return "aec_music_sink", nil
		}
		return "", nil
	}
	done := make(chan struct{})
	go func() {
		c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
		close(done)
	}()
	<-started
	targetCh := make(chan string, 1)
	go func() { targetCh <- c.Target() }()
	select {
	case tgt := <-targetCh:
		if tgt != "aec_music_sink" {
			t.Errorf("Target() = %q while sync blocked in pactl, want aec_music_sink", tgt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Target() blocked while sync should hold no lock")
	}
	close(proceed)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sync did not finish after proceed")
	}
}

// TestSteadyStateSkipsLoadAndSetDefault confirms the steady state re-routes the
// inputs each tick but never reloads the filter or re-asserts the default sink.
func TestSteadyStateSkipsLoadAndSetDefault(t *testing.T) {
	c := New("aec_music_sink")
	rec := &syncRecords{}
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		rec.record(call)
		switch {
		case call == "list short modules":
			return "", nil
		case args[0] == "load-module":
			return "12", nil
		case call == "get-sink-volume clanka_eq_a":
			return "", nil
		case call == "--format=json list sinks":
			return `[{"index":1,"name":"aec_music_sink"},{"index":2,"name":"clanka_eq_a"}]`, nil
		case call == "--format=json list sink-inputs":
			return `[{"index":8,"sink":1,"owner_module":null}]`, nil
		case call == "get-default-sink":
			return "aec_music_sink", nil
		}
		return "", nil
	}
	// First sync applies the profile: one load and one set-default-sink.
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	if n := countContains(strings.Split(rec.joined(), "\n"), "load-module"); n != 1 {
		t.Fatalf("first sync load-module calls = %d, want 1", n)
	}
	if n := countContains(strings.Split(rec.joined(), "\n"), "set-default-sink"); n != 1 {
		t.Fatalf("first sync set-default-sink calls = %d, want 1", n)
	}
	// Second sync is steady state: no load, no set-default-sink, one input listing.
	rec.mu.Lock()
	rec.calls = nil
	rec.mu.Unlock()
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	lines := strings.Split(rec.joined(), "\n")
	if n := countContains(lines, "load-module"); n != 0 {
		t.Errorf("steady load-module calls = %d, want 0", n)
	}
	if n := countContains(lines, "set-default-sink"); n != 0 {
		t.Errorf("steady set-default-sink calls = %d, want 0", n)
	}
	if n := countContains(lines, "--format=json list sink-inputs"); n != 1 {
		t.Errorf("steady sink-input listings = %d, want 1", n)
	}
}

// TestSettingsChangeDuringIOIsNotApplied proves a profile edit that lands while
// sync runs its pactl I/O is not committed; the next tick applies it instead.
func TestSettingsChangeDuringIOIsNotApplied(t *testing.T) {
	c := New("aec_music_sink")
	started := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		once.Do(func() { close(started); <-proceed })
		switch {
		case call == "list short modules":
			return "", nil
		case args[0] == "load-module":
			return "12", nil
		case call == "--format=json list sinks":
			return `[{"index":1,"name":"aec_music_sink"},{"index":2,"name":"clanka_eq_a"}]`, nil
		case call == "--format=json list sink-inputs":
			return `[]`, nil
		case call == "get-default-sink":
			return "aec_music_sink", nil
		}
		return "", nil
	}
	done := make(chan struct{})
	go func() {
		c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
		close(done)
	}()
	<-started
	// Simulate a settings change that lands during the I/O.
	c.mu.Lock()
	c.applied = Encode(Profile{Preset: "Bass Boost"})
	c.mu.Unlock()
	close(proceed)
	<-done
	c.mu.Lock()
	applied := c.applied
	c.mu.Unlock()
	if applied == Encode(Profile{Preset: "Flat"}) {
		t.Fatal("sync committed a profile that changed during its I/O")
	}
	if c.Target() != "clanka_eq_a" {
		t.Fatalf("Target = %q, want clanka_eq_a", c.Target())
	}
	// The next tick applies the new profile.
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	c.mu.Lock()
	applied = c.applied
	c.mu.Unlock()
	if applied != Encode(Profile{Preset: "Flat"}) {
		t.Fatalf("applied = %q after the next tick, want Flat", applied)
	}
}

// TestNoSuchEntityReResolvesDefaultSink covers the non-configured case: when the
// load fails because the learned default sink vanished (a renamed Bluetooth
// sink after a host restart), the controller re-reads the default sink once,
// retries with the new master, and clears Health.
func TestNoSuchEntityReResolvesDefaultSink(t *testing.T) {
	c := New("") // non-configured: master learned from the host default
	rec := &syncRecords{}
	var loadCalls, getDefaultCalls int
	var lmu sync.Mutex
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		rec.record(call)
		switch {
		case call == "list short modules":
			return "", nil
		case call == "get-default-sink":
			lmu.Lock()
			getDefaultCalls++
			n := getDefaultCalls
			lmu.Unlock()
			if n == 1 {
				return "old_sink", nil
			}
			return "new_sink", nil
		case args[0] == "load-module":
			lmu.Lock()
			loadCalls++
			n := loadCalls
			lmu.Unlock()
			if n == 1 {
				return "", errors.New("pactl load-module: exit status 1: Failure: No such entity")
			}
			return "12", nil
		case call == "--format=json list sinks":
			return `[{"index":1,"name":"new_sink"},{"index":2,"name":"clanka_eq_a"}]`, nil
		case call == "--format=json list sink-inputs":
			return `[]`, nil
		}
		return "", nil
	}
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	if c.Health(context.Background()) != nil {
		t.Fatalf("Health should clear after the retry: %v", c.Health(context.Background()))
	}
	if c.Target() != "clanka_eq_a" {
		t.Fatalf("Target = %q, want clanka_eq_a", c.Target())
	}
	if !strings.Contains(rec.joined(), "sink_master=new_sink") {
		t.Fatalf("retry did not use the re-resolved master:\n%s", rec.joined())
	}
	lmu.Lock()
	defer lmu.Unlock()
	if getDefaultCalls < 2 {
		t.Fatalf("get-default-sink calls = %d, want >= 2 (re-resolved after failure)", getDefaultCalls)
	}
}

// TestConfiguredSinkNoExtraGetDefaultOnFailure confirms a configured master
// never triggers the re-resolution: a load failure just falls back flat with no
// extra get-default-sink call.
func TestConfiguredSinkNoExtraGetDefaultOnFailure(t *testing.T) {
	c := New("bluez_output") // configured master
	rec := &syncRecords{}
	var getDefaultCalls int
	var lmu sync.Mutex
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		rec.record(call)
		switch {
		case call == "list short modules":
			return `[]`, nil
		case call == "--format=json list sinks":
			return `[]`, nil
		case call == "--format=json list sink-inputs":
			return `[]`, nil
		case call == "get-default-sink":
			lmu.Lock()
			getDefaultCalls++
			lmu.Unlock()
			return "bluez_output", nil
		case args[0] == "load-module":
			return "", errors.New("pactl load-module: exit status 1: Failure: No such entity")
		}
		return "", nil
	}
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	lmu.Lock()
	n := getDefaultCalls
	lmu.Unlock()
	if n != 0 {
		t.Fatalf("get-default-sink calls = %d, want 0 for a configured sink", n)
	}
	if c.Target() != "bluez_output" {
		t.Fatalf("Target = %q, want bluez_output (flat fallback)", c.Target())
	}
	if c.Health(context.Background()) == nil {
		t.Fatal("Health should report the load failure")
	}
}

// TestRecoveredModuleReResolvesOnFailure exercises a recovered module whose
// master sink has since vanished: recovery learns the (now stale) master, the
// per-tick check drops the dead filter, the load fails with "No such entity",
// and the controller re-resolves the renamed default sink and retries.
func TestRecoveredModuleReResolvesOnFailure(t *testing.T) {
	c := New("") // non-configured: a recovered master is treated as non-configured
	rec := &syncRecords{}
	var loadCalls, getDefaultCalls int
	var lmu sync.Mutex
	c.run = func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		rec.record(call)
		switch call {
		case "list short modules":
			return "7\tmodule-ladspa-sink\tsink_name=clanka_eq_a sink_master=recovered_master plugin=caps\t0", nil
		case "get-sink-volume clanka_eq_a":
			// The recovered sink's master is gone (host restart).
			return "", errors.New("pactl get-sink-volume: No such entity")
		case "get-default-sink":
			// The only get-default-sink call is the re-resolution after the
			// load fails: the host default has since been renamed.
			lmu.Lock()
			getDefaultCalls++
			lmu.Unlock()
			return "renamed_sink", nil
		}
		switch {
		case args[0] == "load-module":
			lmu.Lock()
			loadCalls++
			n := loadCalls
			lmu.Unlock()
			if n == 1 {
				return "", errors.New("pactl load-module: exit status 1: Failure: No such entity")
			}
			return "8", nil
		case call == "--format=json list sinks":
			return `[]`, nil
		case call == "--format=json list sink-inputs":
			return `[]`, nil
		case strings.HasPrefix(call, "unload-module"):
			return "", nil
		}
		return "", nil
	}
	c.sync(context.Background(), Encode(Profile{Preset: "Flat"}))
	if c.Health(context.Background()) != nil {
		t.Fatalf("Health should clear after the retry: %v", c.Health(context.Background()))
	}
	if c.Target() != "clanka_eq_a" {
		t.Fatalf("Target = %q, want clanka_eq_a (the reloaded filter)", c.Target())
	}
	if !strings.Contains(rec.joined(), "sink_master=renamed_sink") {
		t.Fatalf("retry did not use the re-resolved master:\n%s", rec.joined())
	}
	if !strings.Contains(rec.joined(), "unload-module 7") {
		t.Fatalf("stale recovered module not unloaded:\n%s", rec.joined())
	}
	lmu.Lock()
	defer lmu.Unlock()
	if getDefaultCalls < 1 {
		t.Fatalf("get-default-sink calls = %d, want >= 1 (re-resolved after failure)", getDefaultCalls)
	}
}
