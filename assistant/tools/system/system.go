// Package system implements the system.status and system.diagnose Tools: a
// bounded, honest snapshot of the Compose services, the location service and
// every Tool module that declares a health check, plus a private spoken
// diagnosis with the failing checks' details. Both read the same internal/status
// boundaries as the Dashboard Health page.
package system

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical system.status Tool name.
const Name = "system.status"

// DiagnoseName is the canonical system.diagnose Tool name. It speaks the
// failing checks' details on the Pi only; the detail is never model-visible.
const DiagnoseName = "system.diagnose"

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
	OK        bool                `json:"ok"`
	CheckedAt time.Time           `json:"checked_at"`
	Services  []Check             `json:"services,omitempty"`
	Location  *Check              `json:"location,omitempty"`
	Modules   []Check             `json:"modules,omitempty"`
	Host      *tool.HostStatus    `json:"host,omitempty"`
	Storage   *tool.StorageStatus `json:"storage,omitempty"`
	Summary   string              `json:"summary"`
}

// DiagnoseIn has no fields: asking what is wrong needs no arguments.
type DiagnoseIn struct{}

// DiagnoseCheck is one failing check with the detail needed to explain it.
// Unlike Check (used by the model-visible system.status), the Detail is part
// of this Tool's contract; LocalOnlyYes keeps it on the Pi.
type DiagnoseCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// DiagnoseOut is the private diagnosis: up to three failing checks and the
// spoken text built from them.
type DiagnoseOut struct {
	Checks []DiagnoseCheck `json:"checks,omitempty"`
	Text   string          `json:"text"`
}

const (
	// diagnoseMaxChecks bounds how many failures are spoken. The Health page
	// stays the complete view; this is the voice counterpart.
	diagnoseMaxChecks = 3
	// diagnoseDetailRunes bounds one check's Detail in the spoken answer.
	diagnoseDetailRunes = 80
	// diagnoseTextRunes bounds the whole spoken answer.
	diagnoseTextRunes = 300

	diagnoseAllFine     = "Everything I can check is fine."
	diagnoseUnavailable = "I can't check my services right now."
)

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
			"is anything broken",
			"any problems",
		},
	}
}

// NewDiagnose builds system.diagnose. It reuses the one shared status seam; a
// nil seam fails clearly instead of inventing an "everything is fine". A
// Detail can name an internal address or a shortened upstream error, so the
// Tool is Local-only and its result never reaches the model.
func NewDiagnose(run func(context.Context) tool.StatusReport) tool.Tool[DiagnoseIn, DiagnoseOut] {
	return tool.Tool[DiagnoseIn, DiagnoseOut]{
		Name:        DiagnoseName,
		Description: "Say which of the Assistant's services, modules or location checks are failing and why, with a short reason for each. The details stay on this device.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		// Same envelope as system.status: the shared snapshot already has its
		// own 3 s budget, and a timed-out snapshot must still be speakable.
		Timeout: 5 * time.Second,
		Handler: func(ctx context.Context, _ DiagnoseIn) (DiagnoseOut, error) {
			if run == nil {
				return DiagnoseOut{}, &tool.ToolError{Say: diagnoseUnavailable}
			}
			return diagnose(run(ctx)), nil
		},
		Speak: func(out DiagnoseOut) string { return out.Text },
		Examples: []string{
			"what's wrong",
			"what's broken",
			"why are you slow",
			"what failed",
			"which service is down",
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

// Register adds system.status and system.diagnose. Both handlers read
// Deps.Status per call, so the live module list and service checks are always
// current and the two can never disagree.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	if err := tool.Add(r, New(deps.Status)); err != nil {
		return err
	}
	return tool.Add(r, NewDiagnose(deps.Status))
}

// summarize builds the result and the one-line spoken answer. Details stay in
// the Dashboard; a failure names the check, not its internal error text.
func summarize(report tool.StatusReport) Out {
	out := Out{
		CheckedAt: report.CheckedAt,
		Services:  checks(report.Services),
		Modules:   checks(report.Modules),
		Host:      report.Host,
		Storage:   report.Storage,
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
	if warn := vitalsWarning(report); warn != "" {
		out.Summary += " " + warn
	}
	return out
}

// vitalsWarning builds exactly one short spoken warning sentence when a vitals
// threshold (CPU temperature > 80 °C, available memory < 300 MB) or storage
// threshold (free space < 10% or < 500 MB for low, < 2% for full) is crossed.
func vitalsWarning(report tool.StatusReport) string {
	var clauses []string
	if report.Host != nil {
		if report.Host.TemperatureC != nil && *report.Host.TemperatureC > 80.0 {
			clauses = append(clauses, "the CPU is running hot")
		}
		if report.Host.MemoryAvailableBytes != nil && *report.Host.MemoryAvailableBytes < 300*1024*1024 {
			clauses = append(clauses, "available memory is low")
		}
	}
	if report.Storage != nil {
		switch report.Storage.State {
		case "full":
			clauses = append(clauses, "storage space is full")
		case "low":
			clauses = append(clauses, "storage space is running low")
		}
	}
	if len(clauses) == 0 {
		return ""
	}
	return joinClauses(clauses)
}

// joinClauses combines multiple warning clauses into exactly one sentence.
func joinClauses(clauses []string) string {
	var body string
	switch len(clauses) {
	case 1:
		body = clauses[0]
	case 2:
		body = clauses[0] + " and " + clauses[1]
	default:
		body = strings.Join(clauses[:len(clauses)-1], ", ") + " and " + clauses[len(clauses)-1]
	}
	r := []rune(body)
	r[0] = unicode.ToUpper(r[0])
	return string(r) + "."
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

// diagnose builds the private spoken result. It reports at most three failing
// checks with a short reason each, and never claims health from an empty or
// nil snapshot.
func diagnose(report tool.StatusReport) DiagnoseOut {
	if checked(report) == 0 {
		return DiagnoseOut{Text: diagnoseUnavailable}
	}
	failures := failingChecks(report)
	if len(failures) == 0 {
		return DiagnoseOut{Text: diagnoseAllFine}
	}
	if len(failures) > diagnoseMaxChecks {
		failures = failures[:diagnoseMaxChecks]
	}
	out := DiagnoseOut{Checks: make([]DiagnoseCheck, 0, len(failures))}
	parts := make([]string, 0, len(failures))
	for _, item := range failures {
		detail := cleanDetail(item.Detail)
		out.Checks = append(out.Checks, DiagnoseCheck{
			Name:   item.Name,
			State:  string(item.State),
			Detail: detail,
		})
		part := item.Name
		if detail != "" {
			part += ": " + strings.TrimSuffix(detail, ".")
		}
		parts = append(parts, part)
	}
	out.Text = truncateRunes("Here's what's wrong: "+strings.Join(parts, "; ")+".", diagnoseTextRunes)
	return out
}

// failingChecks returns the failing outcomes in the same order as failing().
func failingChecks(report tool.StatusReport) []tool.StatusCheck {
	var failures []tool.StatusCheck
	collect := func(item tool.StatusCheck) {
		if item.State == tool.StatusError || item.State == tool.StatusTimedOut {
			failures = append(failures, item)
		}
	}
	for _, item := range report.Services {
		collect(item)
	}
	for _, item := range report.Modules {
		collect(item)
	}
	collect(report.Location)
	return failures
}

// cleanDetail stops at the first newline and bounds one Detail to the spoken
// limit. Details are bounded by internal/status already; this keeps the voice
// answer short and single-line even if a future producer grows one.
func cleanDetail(detail string) string {
	if i := strings.IndexAny(detail, "\r\n"); i >= 0 {
		detail = detail[:i]
	}
	detail = strings.TrimSpace(detail)
	if runes := []rune(detail); len(runes) > diagnoseDetailRunes {
		detail = strings.TrimSpace(string(runes[:diagnoseDetailRunes]))
	}
	return detail
}

// truncateRunes bounds a string by rune count, not byte count, so multi-byte
// text cannot exceed the spoken limit.
func truncateRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}
