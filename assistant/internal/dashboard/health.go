package dashboard

import (
	"context"
	"fmt"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// healthView is the Health page's whole data model. The page and the JSON
// read under /api/ui/health are rendered from the same view. The check
// results come from internal/status, which the system.status Tool also uses,
// so the page and a spoken status never disagree about a service.
type healthView struct {
	Owner         string
	Page          string
	CheckedAt     string
	HealthTimeout string
	Services      []checkView
	// Host reports Pi host vitals and data-volume storage rows.
	Host []checkView
	// Wakeword has one row per configured model and is probed from the same
	// config the voice loop uses.
	Wakeword []checkView
	// FalseWakes is one row per wake model: the false-wake counts and rate
	// since the process restarted. Nil when voice input is off, which hides
	// the row. Numbers and a model name only — no transcript or private text.
	FalseWakes []falseWakeView
	Location   checkView
	Modules    []checkView
	// Spotify keeps Connect pairing and Web API auth separate; they are
	// different grants and one can be healthy while the other is not.
	Spotify []checkView
	// Routing reports the semantic index's in-memory build state beside the
	// service rows. It is deliberately not part of internal/status: a degraded
	// Fast path must not fail StatusReport.OK() or become a third probe of the
	// same state (the live sampler and system.status Tool share that report).
	Routing checkView
	// DiagnosticsURL is the read-only support download the button on this page
	// requests. It is additive: an older UI ignores it.
	DiagnosticsURL string
}

type checkView struct {
	Name        string
	Status      string
	StatusLabel string
	Detail      string
}

// falseWakeView is one wake model's false-wake measurement since restart. Only
// the model name, counts and a rate string are exposed: never a transcript or
// any private text.
type falseWakeView struct {
	Model      string
	Wakes      int64
	NoSpeech   int64
	Unroutable int64
	Rate       string
}

// health runs the shared checks inside one bounded snapshot and maps them to
// the template's rows.
func (h *Handler) health(ctx context.Context) healthView {
	report := h.healthStatus.Status(ctx)
	return healthView{
		Owner:          h.owner,
		Page:           "health",
		CheckedAt:      report.CheckedAt.Format("2006-01-02 15:04:05 UTC"),
		HealthTimeout:  report.Timeout.String(),
		Services:       checkViews(report.Services),
		Host:           checkViews(report.HostChecks),
		Wakeword:       h.wakewordChecks(ctx),
		FalseWakes:     h.falseWakeRows(),
		Location:       checkViewOf(report.Location),
		Modules:        checkViews(report.Modules),
		Spotify:        h.spotifyChecks(),
		Routing:        h.routingCheck(),
		DiagnosticsURL: "/api/ui/diagnostics",
	}
}

func checkViews(checks []tool.StatusCheck) []checkView {
	views := make([]checkView, 0, len(checks))
	for _, check := range checks {
		views = append(views, checkViewOf(check))
	}
	return views
}

func checkViewOf(check tool.StatusCheck) checkView {
	lbl := check.Label
	if lbl == "" {
		lbl = status.Label(check.State)
	}
	return checkView{
		Name:        check.Name,
		Status:      string(check.State),
		StatusLabel: lbl,
		Detail:      check.Detail,
	}
}

// falseWakeRows maps the voice loop's in-memory counters onto one Health row
// per configured wake model, in config order for stable rendering. The row is
// omitted entirely when voice input is off (no Stats or no wakeword address):
// a disabled loop has no measurement to show, not an error. The stop model
// never starts a turn, so they are not false-wake candidates.
func (h *Handler) falseWakeRows() []falseWakeView {
	if h.falseWakes == nil || strings.TrimSpace(h.wakeword.Addr) == "" {
		return nil
	}
	snapshot := h.falseWakes.Snapshot()
	rows := make([]falseWakeView, 0, len(h.wakeword.Models))
	for _, model := range h.wakeword.Models {
		if model.Command || !model.Enabled {
			continue
		}
		counts := snapshot[model.Name]
		rows = append(rows, falseWakeView{
			Model:      model.Name,
			Wakes:      counts.Wakes,
			NoSpeech:   counts.NoSpeech,
			Unroutable: counts.Unroutable,
			Rate:       counts.Rate(),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// spotifyChecks reports the two Spotify grants independently. It reads only
// status metadata: pairing comes from the Soloist heartbeat file, Web API auth
// from the token manager's last safe result.
func (h *Handler) spotifyChecks() []checkView {
	pairing := spotify.PairingStatus{
		State:  spotify.PairingNotConfigured,
		Label:  "not configured",
		Detail: "Soloist pairing is not configured in this deployment.",
	}
	if h.spotifyPairing != nil {
		pairing = h.spotifyPairing.Status()
	}
	pairingView := checkView{Name: "Connect pairing", Detail: pairing.Detail}
	switch pairing.State {
	case spotify.PairingPaired:
		pairingView.Status = string(tool.StatusOK)
		pairingView.StatusLabel = "paired"
		if pairing.ExpiryKnown && pairing.ExpiresDays <= 14 {
			pairingView.StatusLabel = "paired, update soon"
		}
	case spotify.PairingNotPaired:
		pairingView.Status = string(tool.StatusNotConfigured)
		pairingView.StatusLabel = "not paired"
	case spotify.PairingExpired:
		pairingView.Status = string(tool.StatusError)
		pairingView.StatusLabel = "build expired"
	case spotify.PairingUnavailable:
		pairingView.Status = string(tool.StatusError)
		pairingView.StatusLabel = "unavailable"
	default:
		pairingView.Status = string(tool.StatusNotConfigured)
		pairingView.StatusLabel = pairing.Label
	}

	webAPI := spotify.WebAPIStatus{
		State:  spotify.WebAPINotConfigured,
		Label:  "not configured",
		Detail: "Spotify is not wired in this deployment.",
	}
	if h.spotify != nil {
		webAPI = h.spotify.Status()
	}
	webView := checkView{Name: "Web API auth", Detail: webAPI.Detail}
	switch webAPI.State {
	case spotify.WebAPIConnected:
		webView.Status = string(tool.StatusOK)
		webView.StatusLabel = "connected"
	case spotify.WebAPIReconnect:
		webView.Status = string(tool.StatusError)
		webView.StatusLabel = "reconnect needed"
	case spotify.WebAPIError, spotify.WebAPIStoreUnavailable:
		webView.Status = string(tool.StatusError)
		webView.StatusLabel = "error"
	default:
		webView.Status = string(tool.StatusNotConfigured)
		webView.StatusLabel = webAPI.Label
	}
	return []checkView{pairingView, webView}
}

// routingCheck maps the semantic router's in-memory build state to one Health
// row, like the Spotify rows beside the service probes. It shows tool counts
// and an error string, never Tool output or settings values. A degraded or
// unbuilt Fast path is reported honestly instead of as a green ok, but it does
// not change the shared StatusReport or probe the embed service again.
func (h *Handler) routingCheck() checkView {
	row := checkView{Name: "Semantic routing"}
	if h.examplesState == nil {
		row.Status = string(tool.StatusNotConfigured)
		row.StatusLabel = "not configured"
		row.Detail = "Semantic routing is not configured in this deployment."
		return row
	}

	tools, lastErr, lastAttempt := h.examplesState()
	if lastErr != nil || tools == 0 {
		row.Status = string(tool.StatusError)
		if lastErr != nil {
			row.StatusLabel = "rebuild failed"
			row.Detail = "Last Examples rebuild failed: " + lastErr.Error() + ". Semantic routing is unavailable until a rebuild succeeds."
		} else {
			row.StatusLabel = "not built yet"
			row.Detail = "No Tools are indexed; semantic routing is unavailable until the index is built."
		}
		if !lastAttempt.IsZero() {
			row.Detail += " Last attempt " + lastAttempt.UTC().Format("2006-01-02 15:04:05 UTC") + "."
		}
		return row
	}

	row.Status = string(tool.StatusOK)
	row.StatusLabel = "ok"
	row.Detail = fmt.Sprintf("%d Tools indexed", tools)
	if !lastAttempt.IsZero() {
		row.Detail += "; last rebuilt " + lastAttempt.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	return row
}
