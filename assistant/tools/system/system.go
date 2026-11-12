// Package system implements the system.status Tool: a bounded, honest
// snapshot of the Compose services, the location service and every Tool
// module that declares a health check. It reads the same internal/status
// boundaries as the Dashboard Health page.
package system

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical system.status Tool name.
const Name = "system.status"

// In has no fields: asking how the system is needs no arguments.
type In struct{}

// Check is one named health outcome without its free-text detail, so a
// model-visible result never carries an internal address or stack trace.
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// Out is one bounded snapshot plus the spoken summary built from it.
type Out struct {
	OK        bool      `json:"ok"`
	CheckedAt time.Time `json:"checked_at"`
	Services  []Check   `json:"services,omitempty"`
	Location  *Check    `json:"location,omitempty"`
	Modules   []Check   `json:"modules,omitempty"`
	Summary   string    `json:"summary"`
}

// New builds system.status. A nil status seam makes the Tool fail clearly
// instead of inventing an "everything is fine".
func New(run func(context.Context) tool.StatusReport) tool.Tool[In, Out] {
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Check whether the Assistant's services and Tool modules are healthy.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		// The shared health snapshot has its own 3 s budget; give the Tool a
		// wider envelope so a timed-out snapshot can still be spoken as a
		// truthful "timed out" answer instead of the generic failure line.
		Timeout: 5 * time.Second,
		Handler: func(ctx context.Context, _ In) (Out, error) {
			if run == nil {
				return Out{}, &tool.ToolError{Say: "I can't check my status right now."}
			}
			report := run(ctx)
			return summarize(report), nil
		},
		Speak: func(out Out) string { return out.Summary },
		Examples: []string{
			"are you ok",
			"are you okay",
			"how are you",
			"how are you doing",
			"are you healthy",
			"is everything working",
			"is everything okay",
			"any problems",
		},
	}
}

// NewModule builds the system module.
func NewModule() *Module { return &Module{} }

// Module is the system Tool domain. The status checks are shared seams, not a
// tools.system config block.
type Module struct{}

// Name is the module name and config section suffix.
func (*Module) Name() string { return "system" }

// Register adds system.status. The handler reads Deps.Status per call, so the
// live module list and service checks are always current.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	return tool.Add(r, New(deps.Status))
}

// summarize builds the result and the one-line spoken answer. Details stay in
// the Dashboard; a failure names the check, not its internal error text.
func summarize(report tool.StatusReport) Out {
	out := Out{
		CheckedAt: report.CheckedAt,
		Services:  checks(report.Services),
		Modules:   checks(report.Modules),
	}
	if report.Location.Name != "" {
		location := check(report.Location)
		out.Location = &location
	}
	failed := failing(report)
	checked := checked(report)
	out.OK = report.OK()
	switch {
	case checked == 0:
		out.Summary = "I can't check my services right now."
	case len(failed) == 0:
		out.Summary = "I'm OK. Everything I can check is responding."
	default:
		out.Summary = "I'm having trouble with " + joinNames(failed) + ". " +
			strconv.Itoa(len(failed)) + " " +
			plural(len(failed), "check needs", "checks need") + " attention."
	}
	return out
}

// checked counts the outcomes that can actually fail or pass. A service that
// is not configured and a module with no check are neutral.
func checked(report tool.StatusReport) int {
	count := 0
	for _, item := range report.Services {
		if countable(item) {
			count++
		}
	}
	for _, item := range report.Modules {
		if countable(item) {
			count++
		}
	}
	if countable(report.Location) {
		count++
	}
	return count
}

func countable(check tool.StatusCheck) bool {
	switch check.State {
	case tool.StatusOK, tool.StatusError, tool.StatusTimedOut:
		return true
	default:
		return false
	}
}

func failing(report tool.StatusReport) []string {
	var names []string
	collect := func(check tool.StatusCheck) {
		if check.State == tool.StatusError || check.State == tool.StatusTimedOut {
			names = append(names, check.Name)
		}
	}
	for _, check := range report.Services {
		collect(check)
	}
	for _, check := range report.Modules {
		collect(check)
	}
	collect(report.Location)
	return names
}

func checks(items []tool.StatusCheck) []Check {
	if len(items) == 0 {
		return nil
	}
	out := make([]Check, 0, len(items))
	for _, item := range items {
		out = append(out, check(item))
	}
	return out
}

func check(item tool.StatusCheck) Check {
	return Check{Name: item.Name, State: string(item.State)}
}

// joinNames formats a short list for speech: "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return "nothing"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

func plural(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}
