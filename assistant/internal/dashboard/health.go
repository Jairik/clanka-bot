package dashboard

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

const (
	statusOK            = "ok"
	statusError         = "error"
	statusNotConfigured = "not-configured"
	statusTimedOut      = "timed-out"
	statusNoCheck       = "no-check"
)

// healthView is the Health page's whole data model. The panel is rendered from
// the same view for both a full page load and an htmx refresh.
type healthView struct {
	Owner         string
	CheckedAt     string
	HealthTimeout string
	Services      []checkView
	Location      checkView
	Modules       []checkView
}

type checkView struct {
	Name        string
	Status      string
	StatusLabel string
	Detail      string
}

// health runs the page's checks inside one shared deadline. Service checks run
// concurrently; module checks are one bounded call because the module list
// owns their order and their own per-check budgets.
func (h *Handler) health(ctx context.Context) healthView {
	ctx, cancel := context.WithTimeout(ctx, h.healthTimeout)
	defer cancel()
	return healthView{
		Owner:         h.owner,
		CheckedAt:     h.now().UTC().Format("2006-01-02 15:04:05 UTC"),
		HealthTimeout: h.healthTimeout.String(),
		Services:      h.checkServices(ctx),
		Location:      h.checkLocation(ctx),
		Modules:       h.checkModules(ctx),
	}
}

func (h *Handler) checkServices(ctx context.Context) []checkView {
	views := make([]checkView, len(h.services))
	done := make([]bool, len(h.services))
	pending := 0
	type outcome struct {
		index   int
		err     error
		elapsed time.Duration
	}
	results := make(chan outcome, len(h.services))
	for i, service := range h.services {
		if service.Check == nil {
			views[i] = checkView{
				Name:        service.Name,
				Status:      statusNotConfigured,
				StatusLabel: "not configured",
				Detail:      "This service is not configured in this deployment.",
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
			views[result.index] = serviceResult(h.services[result.index].Name, result.err, result.elapsed, h.healthTimeout, ctx)
			done[result.index] = true
			pending--
		case <-ctx.Done():
			// Abandon checks that ignore the deadline so the page never waits
			// on a wedged container.
			for i, service := range h.services {
				if !done[i] {
					views[i] = timedOutView(service.Name, h.healthTimeout)
				}
			}
			return views
		}
	}
	return views
}

func serviceResult(name string, err error, elapsed time.Duration, timeout time.Duration, ctx context.Context) checkView {
	if err == nil {
		return checkView{Name: name, Status: statusOK, StatusLabel: "ok", Detail: "responded in " + formatDuration(elapsed)}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return timedOutView(name, timeout)
	}
	return checkView{Name: name, Status: statusError, StatusLabel: "error", Detail: shorten(err.Error())}
}

func timedOutView(name string, timeout time.Duration) checkView {
	return checkView{
		Name:        name,
		Status:      statusTimedOut,
		StatusLabel: "timed out",
		Detail:      "No response within " + timeout.String() + ".",
	}
}

func (h *Handler) checkLocation(ctx context.Context) checkView {
	if h.location == nil {
		return checkView{
			Name:        "location",
			Status:      statusNotConfigured,
			StatusLabel: "not configured",
			Detail:      "The location service is not wired.",
		}
	}
	place, known := h.location.Current()
	err := h.location.Health(ctx)
	if err != nil {
		return checkView{Name: "location", Status: statusError, StatusLabel: "error", Detail: shorten(err.Error())}
	}
	if !known {
		return checkView{Name: "location", Status: statusError, StatusLabel: "error", Detail: "No place known."}
	}
	return checkView{Name: "location", Status: statusOK, StatusLabel: "ok", Detail: formatPlace(place)}
}

func (h *Handler) checkModules(ctx context.Context) []checkView {
	if h.modules == nil {
		return []checkView{{
			Name:        "Tool modules",
			Status:      statusNotConfigured,
			StatusLabel: "not configured",
			Detail:      "The Tool module list is not wired.",
		}}
	}
	names := make([]string, 0)
	known := make(map[string]bool)
	for _, module := range h.modules.Modules() {
		names = append(names, module.Name())
		known[module.Name()] = true
	}

	results := make(chan []tools.Health, 1)
	go func() {
		results <- h.modules.Health(ctx)
	}()

	select {
	case health := <-results:
		if ctx.Err() != nil {
			// The deadline fired while the result was in flight; report the
			// timeout rather than mixing a late result into the page.
			return timedOutModules(names, h.healthTimeout)
		}
		byName := make(map[string]error, len(health))
		for _, item := range health {
			byName[item.Module] = item.Err
			if !known[item.Module] {
				known[item.Module] = true
				names = append(names, item.Module)
			}
		}
		if len(names) == 0 {
			return []checkView{{
				Name:        "Tool modules",
				Status:      statusNoCheck,
				StatusLabel: "no checks",
				Detail:      "No Tool module declares a health check.",
			}}
		}
		views := make([]checkView, 0, len(names))
		for _, name := range names {
			err, checked := byName[name]
			switch {
			case !checked:
				views = append(views, checkView{Name: name, Status: statusNoCheck, StatusLabel: "no check", Detail: "No health check declared."})
			case err == nil:
				views = append(views, checkView{Name: name, Status: statusOK, StatusLabel: "ok", Detail: "Health check passed."})
			default:
				views = append(views, checkView{Name: name, Status: statusError, StatusLabel: "error", Detail: shorten(err.Error())})
			}
		}
		return views
	case <-ctx.Done():
		return timedOutModules(names, h.healthTimeout)
	}
}

func timedOutModules(names []string, timeout time.Duration) []checkView {
	if len(names) == 0 {
		return []checkView{{Name: "Tool modules", Status: statusTimedOut, StatusLabel: "timed out", Detail: "No response within " + timeout.String() + "."}}
	}
	views := make([]checkView, 0, len(names))
	for _, name := range names {
		views = append(views, timedOutView(name, timeout))
	}
	return views
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
