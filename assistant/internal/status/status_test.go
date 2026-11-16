package status_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

type fakeLocation struct {
	place location.Location
	known bool
	err   error
}

func (f fakeLocation) Health(context.Context) error { return f.err }
func (f fakeLocation) Current() (location.Location, bool) {
	return f.place, f.known
}

type fakeModules struct {
	names  []string
	health []tools.Health
	block  bool
}

func (f fakeModules) Modules() []tool.Module { return modules(f.names) }

// Health mirrors tools.Set.Health: at the deadline it hands back the checks it
// has and flags each module that did not finish, rather than failing them all.
func (f fakeModules) Health(ctx context.Context) []tools.Health {
	if f.block {
		<-ctx.Done()
		out := make([]tools.Health, 0, len(f.names))
		for _, name := range f.names {
			out = append(out, tools.Health{Module: name, TimedOut: true})
		}
		return out
	}
	return f.health
}

// moduleName is a tool.Module with just a name; Module is an interface, so the
// report only needs the name for a module without a check.
type moduleName string

func (m moduleName) Name() string { return string(m) }
func (moduleName) Register(*tool.Registry, tool.Deps) error {
	return nil
}

func modules(names []string) []tool.Module {
	out := make([]tool.Module, 0, len(names))
	for _, name := range names {
		out = append(out, moduleName(name))
	}
	return out
}

func TestStatusReportsServicesLocationAndModules(t *testing.T) {
	source := status.New(status.Options{
		Timeout: time.Second,
		Services: []status.ServiceCheck{
			{Name: "embed", Check: func(context.Context) error { return nil }},
			{Name: "assistant", Check: func(context.Context) error { return errors.New("dial tcp: connection refused") }},
			{Name: "tts"},
		},
		Location: fakeLocation{
			place: location.Location{City: "Bristol", Region: "England", Timezone: "Europe/London"},
			known: true,
		},
		Modules: fakeModules{
			names: []string{"clock", "weather", "audio"},
			health: []tools.Health{
				{Module: "weather", Err: errors.New("upstream failed")},
				{Module: "audio", Err: nil},
			},
		},
		Now: func() time.Time { return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC) },
	})

	report := source.Status(context.Background())
	if got := report.CheckedAt.Format(time.RFC3339); got != "2026-11-11T15:04:00Z" {
		t.Errorf("CheckedAt = %s", got)
	}
	if report.Timeout != time.Second {
		t.Errorf("Timeout = %s", report.Timeout)
	}
	byName := map[string]tool.StatusCheck{}
	for _, check := range report.Services {
		byName[check.Name] = check
	}
	if byName["embed"].State != tool.StatusOK || !strings.Contains(byName["embed"].Detail, "responded in") {
		t.Errorf("embed = %+v", byName["embed"])
	}
	if byName["assistant"].State != tool.StatusError || !strings.Contains(byName["assistant"].Detail, "connection refused") {
		t.Errorf("assistant = %+v", byName["assistant"])
	}
	if byName["tts"].State != tool.StatusNotConfigured {
		t.Errorf("tts = %+v, want not-configured", byName["tts"])
	}
	if report.Location.State != tool.StatusOK || report.Location.Detail != "Bristol, England (Europe/London)" {
		t.Errorf("location = %+v", report.Location)
	}
	modulesByName := map[string]tool.StatusCheck{}
	for _, check := range report.Modules {
		modulesByName[check.Name] = check
	}
	if modulesByName["clock"].State != tool.StatusNoCheck {
		t.Errorf("clock = %+v, want no-check", modulesByName["clock"])
	}
	if modulesByName["weather"].State != tool.StatusError || !strings.Contains(modulesByName["weather"].Detail, "upstream failed") {
		t.Errorf("weather = %+v", modulesByName["weather"])
	}
	if modulesByName["audio"].State != tool.StatusOK {
		t.Errorf("audio = %+v", modulesByName["audio"])
	}
	if report.OK() {
		t.Error("OK() = true with a failing service and module")
	}
}

func TestStatusWithoutAWiredBoundaryIsNotConfigured(t *testing.T) {
	report := status.New(status.Options{Timeout: time.Second}).Status(context.Background())
	if len(report.Services) != 0 {
		t.Errorf("Services = %+v, want none", report.Services)
	}
	if report.Location.State != tool.StatusNotConfigured {
		t.Errorf("location = %+v, want not-configured", report.Location)
	}
	if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusNotConfigured {
		t.Errorf("modules = %+v, want a not-configured row", report.Modules)
	}
	if report.OK() {
		t.Error("OK() = true without a single check; an empty snapshot is not health evidence")
	}
}

func TestStatusBoundsASlowServiceAndModule(t *testing.T) {
	block := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	source := status.New(status.Options{
		Timeout:  25 * time.Millisecond,
		Services: []status.ServiceCheck{{Name: "embed", Check: block}},
		Modules: fakeModules{
			names: []string{"weather"},
			block: true,
		},
	})

	start := time.Now()
	report := source.Status(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Status took %s, want the 25ms budget to bound it", elapsed)
	}
	if report.Services[0].State != tool.StatusTimedOut {
		t.Errorf("embed = %+v, want timed-out", report.Services[0])
	}
	if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusTimedOut {
		t.Errorf("modules = %+v, want a timed-out row", report.Modules)
	}
}

// TestStatusDoesNotLetASlowServiceStarveTheModules runs the sections
// concurrently: a service that spends the whole budget must not leave the Tool
// modules and the place unchecked.
func TestStatusDoesNotLetASlowServiceStarveTheModules(t *testing.T) {
	block := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	source := status.New(status.Options{
		Timeout:  25 * time.Millisecond,
		Services: []status.ServiceCheck{{Name: "embed", Check: block}},
		Location: fakeLocation{place: location.Location{City: "Bristol"}, known: true},
		Modules:  fakeModules{names: []string{"audio"}, health: []tools.Health{{Module: "audio"}}},
	})

	report := source.Status(context.Background())
	if report.Services[0].State != tool.StatusTimedOut {
		t.Errorf("embed = %+v, want timed-out", report.Services[0])
	}
	if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusOK {
		t.Errorf("modules = %+v, want audio's real ok state", report.Modules)
	}
	if report.Location.State != tool.StatusOK {
		t.Errorf("location = %+v, want its real ok state", report.Location)
	}
}

// lateModules hands back the checks that finished only after the deadline has
// fired, the shape tools.Set.Health now produces.
type lateModules struct {
	names []string
}

func (f lateModules) Modules() []tool.Module { return modules(f.names) }
func (f lateModules) Health(ctx context.Context) []tools.Health {
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	return []tools.Health{
		{Module: "weather", TimedOut: true},
		{Module: "audio", Err: nil},
	}
}

// TestStatusIsolatesATimedOutModule is the #208 regression: one module that
// misses the snapshot deadline must not turn the modules that answered red.
func TestStatusIsolatesATimedOutModule(t *testing.T) {
	source := status.New(status.Options{
		Timeout: 25 * time.Millisecond,
		Modules: lateModules{names: []string{"clock", "weather", "audio"}},
	})

	report := source.Status(context.Background())
	byName := map[string]tool.StatusCheck{}
	for _, check := range report.Modules {
		byName[check.Name] = check
	}
	if byName["weather"].State != tool.StatusTimedOut {
		t.Errorf("weather = %+v, want timed-out", byName["weather"])
	}
	if byName["audio"].State != tool.StatusOK || byName["audio"].Detail != "Health check passed." {
		t.Errorf("audio = %+v, want its real ok state", byName["audio"])
	}
	if byName["clock"].State != tool.StatusNoCheck {
		t.Errorf("clock = %+v, want no-check", byName["clock"])
	}
	if want := []string{"clock", "weather", "audio"}; strings.Join(namesOf(report.Modules), ",") != strings.Join(want, ",") {
		t.Errorf("module order = %v, want %v", namesOf(report.Modules), want)
	}
}

// gatedModules counts the shared passes and lets the test hold one open.
type gatedModules struct {
	calls   atomic.Int32
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newGatedModules() *gatedModules {
	return &gatedModules{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedModules) Modules() []tool.Module { return modules([]string{"clock"}) }
func (g *gatedModules) Health(context.Context) []tools.Health {
	if g.calls.Add(1) == 1 {
		g.once.Do(func() { close(g.started) })
	}
	<-g.release
	return []tools.Health{{Module: "clock", Err: nil}}
}

// TestStatusSingleFlightsConcurrentCallers proves the page, its sampler and a
// spoken status share one check pass instead of repeating every probe.
func TestStatusSingleFlightsConcurrentCallers(t *testing.T) {
	gated := newGatedModules()
	source := status.New(status.Options{Timeout: 5 * time.Second, Modules: gated})

	const callers = 5
	reports := make([]tool.StatusReport, callers)
	var wg sync.WaitGroup
	for i := range reports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reports[i] = source.Status(context.Background())
		}(i)
	}
	<-gated.started
	close(gated.release)
	wg.Wait()

	if got := gated.calls.Load(); got != 1 {
		t.Fatalf("Health passes = %d for %d concurrent callers, want 1", got, callers)
	}
	for i, report := range reports {
		if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusOK {
			t.Fatalf("caller %d report = %+v, want the shared ok snapshot", i, report.Modules)
		}
		if !report.CheckedAt.Equal(reports[0].CheckedAt) {
			t.Errorf("caller %d CheckedAt = %s, want the shared pass's %s", i, report.CheckedAt, reports[0].CheckedAt)
		}
	}
}

func TestStatusReusesASnapshotForTheShareTTLThenRefreshes(t *testing.T) {
	gated := newGatedModules()
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	source := status.New(status.Options{
		Timeout: time.Second,
		Modules: gated,
		Now:     func() time.Time { return now },
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		source.Status(context.Background())
	}()
	<-gated.started
	close(gated.release)
	<-done

	// A call inside the TTL is answered by the finished snapshot.
	source.Status(context.Background())
	if got := gated.calls.Load(); got != 1 {
		t.Fatalf("Health passes = %d inside the share TTL, want the cached snapshot", got)
	}

	// Just after the TTL the next caller starts one fresh pass.
	now = now.Add(status.SharedSnapshotTTL + time.Second)
	gated.release = make(chan struct{})
	close(gated.release)
	source.Status(context.Background())
	if got := gated.calls.Load(); got != 2 {
		t.Fatalf("Health passes = %d after the share TTL, want a refresh", got)
	}
}

// TestStatusKeepsTheSharedPassWhenTheFirstCallerGoesAway proves the pass runs on
// its own context: a cancelled HTTP request must not report timed out for the
// caller that joined it.
func TestStatusKeepsTheSharedPassWhenTheFirstCallerGoesAway(t *testing.T) {
	gated := newGatedModules()
	source := status.New(status.Options{Timeout: 5 * time.Second, Modules: gated})

	cancelled, cancel := context.WithCancel(context.Background())
	first := make(chan tool.StatusReport, 1)
	go func() { first <- source.Status(cancelled) }()
	<-gated.started

	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	joined := make(chan tool.StatusReport, 1)
	go func() { joined <- source.Status(second) }()
	cancel()

	close(gated.release)
	for name, report := range map[string]tool.StatusReport{"cancelled caller": <-first, "joined caller": <-joined} {
		if len(report.Modules) != 1 || report.Modules[0].State != tool.StatusOK {
			t.Errorf("%s modules = %+v, want the shared ok snapshot", name, report.Modules)
		}
	}
	if got := gated.calls.Load(); got != 1 {
		t.Errorf("Health passes = %d, want one shared pass", got)
	}
}

// overrunModules ignores the snapshot deadline and hands back its results only
// after more than moduleGrace has elapsed (the abandoned-list case from #360).
type overrunModules struct {
	names []string
	delay time.Duration
}

func (f overrunModules) Modules() []tool.Module { return modules(f.names) }
func (f overrunModules) Health(ctx context.Context) []tools.Health {
	<-ctx.Done()
	time.Sleep(f.delay)
	return []tools.Health{{Module: "clock", Err: nil}}
}

// TestStatusAbandonedModuleListTimesOutEveryModule is the #360 regression: a
// list that overruns the module-list grace must not report its modules as
// no-check (which OK() would ignore), but as timed out, keeping the report
// honest even when another boundary passed.
func TestStatusAbandonedModuleListTimesOutEveryModule(t *testing.T) {
	// moduleGrace is 200ms; sleeping past it forces the abandoned-list path.
	const overrun = 400 * time.Millisecond
	source := status.New(status.Options{
		Timeout:  25 * time.Millisecond,
		Services: []status.ServiceCheck{{Name: "assistant", Check: func(context.Context) error { return nil }}},
		Modules:  overrunModules{names: []string{"clock", "weather", "audio"}, delay: overrun},
	})

	start := time.Now()
	report := source.Status(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Status took %s, want the grace to bound the abandoned list", elapsed)
	}
	if len(report.Modules) != 3 {
		t.Fatalf("modules = %+v, want a row per module", report.Modules)
	}
	for _, check := range report.Modules {
		if check.State != tool.StatusTimedOut {
			t.Errorf("%s = %+v, want timed-out", check.Name, check)
		}
	}
	if report.OK() {
		t.Error("OK() = true with no module checked; an abandoned list must not read healthy")
	}
}

// TestStatusListReturningASubsetKeepsNoCheckForAbsent proves only a list that
// actually returned, with a name genuinely missing from its results, reads
// no-check.
func TestStatusListReturningASubsetKeepsNoCheckForAbsent(t *testing.T) {
	source := status.New(status.Options{
		Timeout: time.Second,
		Modules: fakeModules{
			names:  []string{"clock", "weather", "audio"},
			health: []tools.Health{{Module: "weather", Err: nil}, {Module: "audio", Err: nil}},
		},
	})

	report := source.Status(context.Background())
	byName := map[string]tool.StatusCheck{}
	for _, check := range report.Modules {
		byName[check.Name] = check
	}
	if got := byName["clock"]; got.State != tool.StatusNoCheck || got.Detail != "No health check declared." {
		t.Errorf("clock = %+v, want no-check for the absent name", got)
	}
	if byName["weather"].State != tool.StatusOK || byName["audio"].State != tool.StatusOK {
		t.Errorf("returned modules lost their real states: %+v", report.Modules)
	}
}

func namesOf(checks []tool.StatusCheck) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
	}
	return names
}

func TestStatusNeverFabricatesAPlace(t *testing.T) {
	report := status.New(status.Options{
		Timeout:  time.Second,
		Location: fakeLocation{known: false},
	}).Status(context.Background())
	if report.Location.State != tool.StatusError || report.Location.Detail != "No place known." {
		t.Fatalf("location = %+v, want the honest no-place error", report.Location)
	}
}

func TestLabels(t *testing.T) {
	cases := map[tool.StatusState]string{
		tool.StatusOK:            "ok",
		tool.StatusError:         "error",
		tool.StatusNotConfigured: "not configured",
		tool.StatusTimedOut:      "timed out",
		tool.StatusNoCheck:       "no check",
	}
	for state, want := range cases {
		if got := status.Label(state); got != want {
			t.Errorf("Label(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestSeparateModuleCheckKeepsItsOwnStateAndNeverFailsTheReport(t *testing.T) {
	source := status.New(status.Options{
		Timeout: time.Second,
		Modules: fakeModules{
			names: []string{"github"},
			health: []tools.Health{
				{Module: "github"},
				{Module: "github notifications", Check: &tool.StatusCheck{
					Name: "github notifications", State: tool.StatusNotConfigured, Detail: "needs a classic token",
				}},
			},
		},
	})
	report := source.Status(context.Background())
	byName := map[string]tool.StatusCheck{}
	for _, check := range report.Modules {
		byName[check.Name] = check
	}
	if byName["github"].State != tool.StatusOK {
		t.Errorf("github = %+v, want ok", byName["github"])
	}
	if got := byName["github notifications"]; got.State != tool.StatusNotConfigured || got.Detail != "needs a classic token" {
		t.Errorf("github notifications = %+v, want its own not-configured row", got)
	}
	if !report.OK() {
		t.Error("OK() = false; an unset optional secret must not make the report unhealthy")
	}
}
