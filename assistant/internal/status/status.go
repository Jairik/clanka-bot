// Package status runs the shared health boundaries in one bounded snapshot.
// The Dashboard's Health page and the system.status Tool both read it, so a
// service or Tool module is checked the same way everywhere. A check that
// overruns the budget is reported as timed out; an unreachable upstream is an
// error, never a fabricated "ok".
package status

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

// DefaultTimeout matches the Dashboard's shipped health_timeout.
const DefaultTimeout = 3 * time.Second

const (
	// SharedSnapshotTTL is how long a finished snapshot answers later callers.
	// It is short on purpose: a spoken system.status right after a page load is
	// at most this stale, while the page, its live sampler and a spoken request
	// still share one pass.
	SharedSnapshotTTL = 2 * time.Second
	// moduleGrace is how long the module list may take to hand back the checks
	// it had already finished once the deadline fired. The Tool set answers
	// immediately; a custom list that ignores the deadline is abandoned after
	// it, so the snapshot never waits on a wedged check.
	moduleGrace = 200 * time.Millisecond
)

// ServiceCheck is one Compose service's health check. A nil Check means the
// service is not configured in this deployment, which is not a failure.
type ServiceCheck struct {
	Name  string
	Check func(context.Context) error
}

// Modules is the Tool module list and its health checks.
type Modules interface {
	Modules() []tool.Module
	Health(context.Context) []tools.Health
}

// Location is the place service the report includes.
type Location interface {
	Health(context.Context) error
	Current() (location.Location, bool)
}

// Options configures one snapshot. Now is injectable for deterministic tests.
type Options struct {
	Timeout  time.Duration
	Services []ServiceCheck
	Modules  Modules
	Location Location
	Now      func() time.Time
}

// Source owns the shared checks and their overall deadline. One source is
// shared by the Dashboard and the system.status Tool, so concurrent callers
// join one in-flight pass and a just-finished snapshot is reused.
type Source struct {
	timeout  time.Duration
	services []ServiceCheck
	modules  Modules
	location Location
	now      func() time.Time

	mu       sync.Mutex
	last     tool.StatusReport
	lastAt   time.Time
	lastSet  bool
	inflight *snapshotCall
}

// snapshotCall is one shared pass in flight; waiters block on done and read the
// report the pass recorded.
type snapshotCall struct {
	done   chan struct{}
	report tool.StatusReport
}

// New builds a status source. A zero or negative timeout uses DefaultTimeout.
func New(opts Options) *Source {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Source{
		timeout:  timeout,
		services: opts.Services,
		modules:  opts.Modules,
		location: opts.Location,
		now:      now,
	}
}

// Status returns the shared snapshot. Concurrent callers join one in-flight
// pass, and a snapshot younger than SharedSnapshotTTL is reused, so a page
// render, its live sampler and a spoken status never repeat the same probes.
// The pass runs on its own timeout-bounded context: a caller that goes away
// cannot fail the snapshot the other callers are waiting for. The returned
// report may be shared with later callers, so treat it as read-only.
func (s *Source) Status(ctx context.Context) tool.StatusReport {
	if s == nil {
		return tool.StatusReport{CheckedAt: time.Now().UTC(), Timeout: DefaultTimeout}
	}

	s.mu.Lock()
	if s.lastSet && s.now().Sub(s.lastAt) < SharedSnapshotTTL {
		report := s.last
		s.mu.Unlock()
		return report
	}
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		// The shared pass is bounded by the snapshot timeout, so this wait is
		// bounded too; repeating the checks for one caller is what the cache
		// exists to avoid.
		<-call.done
		return call.report
	}
	call := &snapshotCall{done: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	// Detach from the first caller's request context. Its deadline still bounds
	// the pass through snapshot, but a cancelled HTTP request must not report
	// "timed out" for the callers that joined later.
	report := s.snapshot(context.WithoutCancel(ctx))

	s.mu.Lock()
	s.last, s.lastAt, s.lastSet = report, s.now(), true
	call.report = report
	s.inflight = nil
	close(call.done)
	s.mu.Unlock()
	return report
}

// snapshot runs every section of the report inside one shared deadline. The
// sections run concurrently, so a slow service probe cannot starve the Tool
// modules of the whole budget before they even start.
func (s *Source) snapshot(ctx context.Context) tool.StatusReport {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var (
		services []tool.StatusCheck
		location tool.StatusCheck
		modules  []tool.StatusCheck
		wg       sync.WaitGroup
	)
	wg.Add(3)
	go func() { defer wg.Done(); services = s.checkServices(ctx) }()
	go func() { defer wg.Done(); location = s.checkLocation(ctx) }()
	go func() { defer wg.Done(); modules = s.checkModules(ctx) }()
	wg.Wait()

	return tool.StatusReport{
		CheckedAt: s.now().UTC(),
		Timeout:   s.timeout,
		Services:  services,
		Location:  location,
		Modules:   modules,
	}
}

func (s *Source) checkServices(ctx context.Context) []tool.StatusCheck {
	views := make([]tool.StatusCheck, len(s.services))
	done := make([]bool, len(s.services))
	pending := 0
	type outcome struct {
		index   int
		err     error
		elapsed time.Duration
	}
	results := make(chan outcome, len(s.services))
	for i, service := range s.services {
		if service.Check == nil {
			views[i] = tool.StatusCheck{
				Name:   service.Name,
				State:  tool.StatusNotConfigured,
				Detail: "This service is not configured in this deployment.",
			}
			done[i] = true
			continue
		}
		pending++
		go func(i int, check func(context.Context) error) {
			start := time.Now()
			err := check(ctx)
			results <- outcome{index: i, err: err, elapsed: time.Since(start)}
		}(i, service.Check)
	}

	for pending > 0 {
		select {
		case result := <-results:
			views[result.index] = serviceResult(s.services[result.index].Name, result.err, result.elapsed, s.timeout, ctx)
			done[result.index] = true
			pending--
		case <-ctx.Done():
			// Abandon checks that ignore the deadline so the snapshot never
			// waits on a wedged container.
			for i, service := range s.services {
				if !done[i] {
					views[i] = timedOutCheck(service.Name, s.timeout)
				}
			}
			return views
		}
	}
	return views
}

// serviceResult turns one completed check into a view. A deadline error is a
// timeout even when the check returns it as a plain error.
func serviceResult(name string, err error, elapsed time.Duration, timeout time.Duration, ctx context.Context) tool.StatusCheck {
	if err == nil {
		return tool.StatusCheck{Name: name, State: tool.StatusOK, Detail: "responded in " + formatDuration(elapsed)}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return timedOutCheck(name, timeout)
	}
	return tool.StatusCheck{Name: name, State: tool.StatusError, Detail: shorten(err.Error())}
}

func timedOutCheck(name string, timeout time.Duration) tool.StatusCheck {
	return tool.StatusCheck{
		Name:   name,
		State:  tool.StatusTimedOut,
		Detail: "No response within " + timeout.String() + ".",
	}
}

func (s *Source) checkLocation(ctx context.Context) tool.StatusCheck {
	if s.location == nil {
		return tool.StatusCheck{
			Name:   "location",
			State:  tool.StatusNotConfigured,
			Detail: "The location service is not wired.",
		}
	}
	place, known := s.location.Current()
	done := make(chan error, 1)
	go func() { done <- s.location.Health(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		// The place lookup ignores the deadline in a wedged deployment; report
		// it alone rather than waiting on it.
		return timedOutCheck("location", s.timeout)
	}
	if err != nil {
		return tool.StatusCheck{Name: "location", State: tool.StatusError, Detail: shorten(err.Error())}
	}
	if !known {
		return tool.StatusCheck{Name: "location", State: tool.StatusError, Detail: "No place known."}
	}
	return tool.StatusCheck{Name: "location", State: tool.StatusOK, Detail: formatPlace(place)}
}

func (s *Source) checkModules(ctx context.Context) []tool.StatusCheck {
	if s.modules == nil {
		return []tool.StatusCheck{{
			Name:   "Tool modules",
			State:  tool.StatusNotConfigured,
			Detail: "The Tool module list is not wired.",
		}}
	}
	names := make([]string, 0)
	for _, module := range s.modules.Modules() {
		names = append(names, module.Name())
	}

	results := make(chan []tools.Health, 1)
	go func() {
		results <- s.modules.Health(ctx)
	}()

	var health []tools.Health
	graceExpired := false
	select {
	case health = <-results:
	case <-ctx.Done():
		// Every module now answers for itself, so the list hands back the
		// checks that finished at the deadline with a TimedOut flag on the rest.
		// Give it that moment; a list that ignores the deadline is abandoned.
		timer := time.NewTimer(moduleGrace)
		defer timer.Stop()
		select {
		case health = <-results:
		case <-timer.C:
			graceExpired = true
			// A check that landed just as the timer fired is still usable.
			// The channel is buffered and gets exactly one send, so this
			// never blocks and keeps whichever results did arrive.
			select {
			case health = <-results:
			default:
			}
		}
	}
	return moduleChecks(names, health, s.timeout, graceExpired)
}

// moduleChecks maps per-module results to the report's rows, keeping the module
// order. A module the list marked TimedOut, or one left without a result after
// the list's grace expired, reads as timed out; every module that answered keeps
// its own real state. Only a list that actually returned, with a name genuinely
// absent, reads as no-check.
func moduleChecks(names []string, health []tools.Health, timeout time.Duration, graceExpired bool) []tool.StatusCheck {
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	byName := make(map[string]error, len(health))
	separate := make(map[string]tool.StatusCheck)
	timedOut := make(map[string]bool)
	for _, item := range health {
		if !known[item.Module] {
			known[item.Module] = true
			names = append(names, item.Module)
		}
		if item.TimedOut {
			timedOut[item.Module] = true
			continue
		}
		if item.Check != nil {
			separate[item.Module] = *item.Check
		}
		byName[item.Module] = item.Err
	}
	if len(names) == 0 {
		return []tool.StatusCheck{{
			Name:   "Tool modules",
			State:  tool.StatusNoCheck,
			Detail: "No Tool module declares a health check.",
		}}
	}
	views := make([]tool.StatusCheck, 0, len(names))
	for _, name := range names {
		if timedOut[name] {
			views = append(views, timedOutCheck(name, timeout))
			continue
		}
		if check, ok := separate[name]; ok {
			check.Detail = shorten(check.Detail)
			views = append(views, check)
			continue
		}
		err, checked := byName[name]
		switch {
		case !checked:
			// The list was abandoned after its grace expired, so a missing
			// result means the check never came back — not that no check was
			// declared. Keep the report honest and OK() false.
			if graceExpired {
				views = append(views, timedOutCheck(name, timeout))
			} else {
				views = append(views, tool.StatusCheck{Name: name, State: tool.StatusNoCheck, Detail: "No health check declared."})
			}
		case err == nil:
			views = append(views, tool.StatusCheck{Name: name, State: tool.StatusOK, Detail: "Health check passed."})
		default:
			views = append(views, tool.StatusCheck{Name: name, State: tool.StatusError, Detail: shorten(err.Error())})
		}
	}
	return views
}

// Label is the human spelling of one state, shared by the Dashboard and the
// spoken status summary.
func Label(state tool.StatusState) string {
	switch state {
	case tool.StatusOK:
		return "ok"
	case tool.StatusError:
		return "error"
	case tool.StatusNotConfigured:
		return "not configured"
	case tool.StatusTimedOut:
		return "timed out"
	case tool.StatusNoCheck:
		return "no check"
	default:
		return string(state)
	}
}

func formatPlace(place location.Location) string {
	name := strings.TrimSpace(place.City)
	if region := strings.TrimSpace(place.Region); region != "" {
		if name != "" {
			name += ", "
		}
		name += region
	}
	if timezone := strings.TrimSpace(place.Timezone); timezone != "" {
		name += " (" + timezone + ")"
	}
	if name == "" {
		return "No place known."
	}
	return name
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "under 1ms"
	}
	return d.Round(time.Millisecond).String()
}

func shorten(text string) string {
	const max = 300
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}
