package dashboard

import (
	"context"

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
	// Wakeword has one row per configured model and is probed from the same
	// config the voice loop uses.
	Wakeword []checkView
	Location checkView
	Modules  []checkView
	// Spotify keeps Connect pairing and Web API auth separate; they are
	// different grants and one can be healthy while the other is not.
	Spotify []checkView
}

type checkView struct {
	Name        string
	Status      string
	StatusLabel string
	Detail      string
}

// health runs the shared checks inside one bounded snapshot and maps them to
// the template's rows.
func (h *Handler) health(ctx context.Context) healthView {
	report := h.healthStatus.Status(ctx)
	return healthView{
		Owner:         h.owner,
		Page:          "health",
		CheckedAt:     report.CheckedAt.Format("2006-01-02 15:04:05 UTC"),
		HealthTimeout: report.Timeout.String(),
		Services:      checkViews(report.Services),
		Wakeword:      h.wakewordChecks(ctx),
		Location:      checkViewOf(report.Location),
		Modules:       checkViews(report.Modules),
		Spotify:       h.spotifyChecks(),
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
	return checkView{
		Name:        check.Name,
		Status:      string(check.State),
		StatusLabel: status.Label(check.State),
		Detail:      check.Detail,
	}
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
