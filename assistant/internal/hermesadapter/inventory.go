package hermesadapter

import "github.com/Jairik/clanka-bot/assistant/internal/tool"

// Allowlist is the smallest read-only Tool set Hermes may discover. Both are
// weather reads: they have no side effects, they are not Local-only, and they
// share the WeatherAPI account grant. spotify.now_playing was the other
// suggested sample, but it is Local-only because it names the owner's
// listening, so ADR-0002 keeps it off this surface.
var Allowlist = []string{
	"weather.current",
	"weather.forecast",
}

// Decision is the product call for one Go Tool relative to Hermes.
type Decision string

const (
	// DecisionAllowlist is on the current enforced surface (once granted).
	DecisionAllowlist Decision = "allowlist"
	// DecisionLater is Agent-reachable and can join the same adapter later.
	DecisionLater Decision = "later"
	// DecisionLocalOnly stays on the Pi; Hermes must never see it.
	DecisionLocalOnly Decision = "local_only"
	// DecisionConfirm is an outward write; Go owns Confirmation.
	DecisionConfirm Decision = "confirm"
)

// Notes record explicit product calls that the default rule would not show.
var Notes = map[string]string{
	"spotify.now_playing":  "Suggested as a second sample; Local-only because it names the owner's listening, so it stays off Hermes.",
	"github.pr_list":       "Read, not Local-only; later via this adapter, not the first two-tool allowlist.",
	"github.issue_create":  "Outward write. Go-owned Confirmation; the adapter never proposes or runs it.",
	"github.notifications": "Read, not Local-only; later via this adapter once the GitHub grant is in the allowlist.",
	"github.checks":        "Read, not Local-only; later via this adapter. The account catalog still lists issue, PR and notifications.",
	"web.search":           "Agent-only read; later via this adapter. No Hermes grant of its own.",
	"web.fetch":            "Agent-only read; later via this adapter, still behind the public-URL provenance rule.",
	"note.recall":          "Agent-visible by ADR-0005; later via this adapter, not the first allowlist.",
	"list.read":            "Agent-visible by ADR-0005; later via this adapter, not the first allowlist.",
}

// Classify is the product decision for one registered Tool. Allowlisted names
// must still be read, enabled and not Local-only at runtime; those live gates
// sit in Adapter.inspect, not here.
func Classify(info tool.Info) Decision {
	if Allowlisted(info.Name) {
		return DecisionAllowlist
	}
	if info.LocalOnly {
		return DecisionLocalOnly
	}
	if info.Permission == tool.PermissionConfirm {
		return DecisionConfirm
	}
	return DecisionLater
}

// Allowlisted reports whether name is one of the two shipped samples.
func Allowlisted(name string) bool {
	for _, allowed := range Allowlist {
		if allowed == name {
			return true
		}
	}
	return false
}
