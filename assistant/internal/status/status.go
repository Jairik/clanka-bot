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
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

// DefaultTimeout matches the Dashboard's shipped health_timeout.
const DefaultTimeout = 3 * time.Second

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

// Source owns the shared checks and their overall deadline.
type Source struct {
	timeout  time.Duration
	services []ServiceCheck
	modules  Modules
	location Location
	now      func() time.Time
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

// Status runs every check inside one shared deadline. Service checks run
// concurrently; module checks are one call because the module list owns their
// order and each module's own budget.
func (s *Source) Status(ctx context.Context) tool.StatusReport {
	if s == nil {
		return tool.StatusReport{CheckedAt: time.Now().UTC(), Timeout: DefaultTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return tool.StatusReport{
		CheckedAt: s.now().UTC(),
		Timeout:   s.timeout,
		Services:  s.checkServices(ctx),
		Location:  s.checkLocation(ctx),
		Modules:   s.checkModules(ctx),
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
	if err := s.location.Health(ctx); err != nil {
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
	known := make(map[string]bool)
	for _, module := range s.modules.Modules() {
		names = append(names, module.Name())
		known[module.Name()] = true
	}

	results := make(chan []tools.Health, 1)
	go func() {
		results <- s.modules.Health(ctx)
	}()

	select {
	case health := <-results:
		if ctx.Err() != nil {
			// The deadline fired while the result was in flight; report the
			// timeout rather than mixing a late result into the snapshot.
			return timedOutModules(names, s.timeout)
		}
		byName := make(map[string]error, len(health))
		separate := make(map[string]tool.StatusCheck)
		for _, item := range health {
			if item.Check != nil {
				separate[item.Module] = *item.Check
			}
			byName[item.Module] = item.Err
			if !known[item.Module] {
				known[item.Module] = true
				names = append(names, item.Module)
			}
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
			err, checked := byName[name]
			if check, ok := separate[name]; ok {
				check.Detail = shorten(check.Detail)
				views = append(views, check)
				continue
			}
			switch {
			case !checked:
				views = append(views, tool.StatusCheck{Name: name, State: tool.StatusNoCheck, Detail: "No health check declared."})
			case err == nil:
				views = append(views, tool.StatusCheck{Name: name, State: tool.StatusOK, Detail: "Health check passed."})
			default:
				views = append(views, tool.StatusCheck{Name: name, State: tool.StatusError, Detail: shorten(err.Error())})
			}
		}
		return views
	case <-ctx.Done():
		return timedOutModules(names, s.timeout)
	}
}

func timedOutModules(names []string, timeout time.Duration) []tool.StatusCheck {
	if len(names) == 0 {
		return []tool.StatusCheck{{Name: "Tool modules", State: tool.StatusTimedOut, Detail: "No response within " + timeout.String() + "."}}
	}
	views := make([]tool.StatusCheck, 0, len(names))
	for _, name := range names {
		views = append(views, timedOutCheck(name, timeout))
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
