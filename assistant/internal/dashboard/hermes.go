package dashboard

import (
	"net/http"
	"sort"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// Hermes page flash values.
const (
	flashHermesLinked   = "hermes-linked"
	flashHermesUnlinked = "hermes-unlinked"
	flashHermesGranted  = "hermes-granted"
	flashHermesRevoked  = "hermes-revoked"
)

// routesView is the Routes page: one row per Tool, saying whether the Fast
// path may choose it, whether only the Agent may call it, and whether it stays
// on the Pi. The counts make the deterministic surface explicit.
type routesView struct {
	Owner       string
	Page        string
	Flash       string
	Error       string
	HasRegistry bool
	Prefixes    []string
	Filler      string
	Unavailable string
	Totals      routesTotals
	Rows        []routeRowView
}

type routesTotals struct {
	Tools     int
	FastPath  int
	AgentOnly int
	LocalOnly int
	Confirm   int
	Hermes    int
}

type routeRowView struct {
	Name           string
	Description    string
	Permission     string
	FastPath       bool
	AgentOnly      bool
	LocalOnly      bool
	Confirm        bool
	Enabled        bool
	Hermes         bool
	ControlPhrases int
	Examples       int
}

// hermesView is the Hermes control panel: what Hermes would see, and which
// linked accounts it may use. Hermes itself is not installed by this build, so
// the page says that plainly instead of pretending a job runner exists.
type hermesView struct {
	Owner           string
	Page            string
	Flash           string
	Error           string
	HasRegistry     bool
	HasIntegrations bool
	Surface         []hermesToolView
	SurfaceCount    int
	LocalOnlyCount  int
	ConfirmCount    int
	Granted         int
	Rows            []integrationView
}

type hermesToolView struct {
	Name        string
	Description string
	Permission  string
}

type integrationView struct {
	ID        string
	Name      string
	Summary   string
	Docs      string
	Status    string
	StatusKey string
	Hermes    bool
	CanGrant  bool
	Linkable  bool
	Secrets   []integrationSecretView
	Tools     []string
}

type integrationSecretView struct {
	Name     string
	Set      bool
	Source   string
	Last4    string
	Optional bool
}

// routesPage renders the deterministic/Agent/Local-only split.
func (h *Handler) routesPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildRoutesView("", ""))
}

// hermesPage renders the tool surface and the linked accounts.
func (h *Handler) hermesPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildHermesView("", ""))
}

// hermesLink stores one account's secrets. The form carries one field per
// secret; unknown fields are refused by the integrations service.
func (h *Handler) hermesLink(w http.ResponseWriter, r *http.Request) {
	if h.integrations == nil {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.writeJSON(w, http.StatusBadRequest, h.buildHermesView("", "Could not read the form."))
		return
	}
	id := r.PathValue("id")
	view, ok := h.integrations.Get(id)
	if !ok {
		h.writeJSON(w, http.StatusNotFound, h.buildHermesView("", "That account does not exist."))
		return
	}
	values := make(map[string]string, len(view.Secrets))
	for _, secret := range view.Secrets {
		values[secret.Name] = r.FormValue(secret.Name)
	}
	if err := h.integrations.Link(r.Context(), id, values); err != nil {
		h.logger.Warn("integration link failed", "id", id, "error", err.Error())
		h.writeJSON(w, http.StatusBadRequest, h.buildHermesView("", integrationLinkMessage(err)))
		return
	}
	h.logger.Info("integration linked from dashboard", "id", id)
	h.writeJSON(w, http.StatusOK, h.buildHermesView(flashHermesLinked, ""))
}

// hermesUnlink clears every secret an account owns.
func (h *Handler) hermesUnlink(w http.ResponseWriter, r *http.Request) {
	if h.integrations == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	if err := h.integrations.Unlink(r.Context(), id); err != nil {
		h.logger.Warn("integration unlink failed", "id", id, "error", err.Error())
		h.writeJSON(w, http.StatusBadRequest, h.buildHermesView("", "The account could not be unlinked."))
		return
	}
	h.logger.Info("integration unlinked from dashboard", "id", id)
	h.writeJSON(w, http.StatusOK, h.buildHermesView(flashHermesUnlinked, ""))
}

// hermesGrant toggles one account's Hermes access. The grant is a live
// setting; it stores intent until the Hermes adapter exists.
func (h *Handler) hermesGrant(w http.ResponseWriter, r *http.Request) {
	if h.integrations == nil {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.writeJSON(w, http.StatusBadRequest, h.buildHermesView("", "Could not read the form."))
		return
	}
	id := r.PathValue("id")
	allowed := strings.TrimSpace(r.FormValue("allowed")) == "on"
	if err := h.integrations.SetGrant(r.Context(), id, allowed); err != nil {
		h.logger.Warn("integration grant failed", "id", id, "error", err.Error())
		h.writeJSON(w, http.StatusBadRequest, h.buildHermesView("", "The grant could not be changed."))
		return
	}
	flash := flashHermesRevoked
	if allowed {
		flash = flashHermesGranted
	}
	h.writeJSON(w, http.StatusOK, h.buildHermesView(flash, ""))
}

// buildRoutesView precomputes the whole Tool table. Control-phrase counts are
// the live settings, not the code defaults, so the page matches what routing
// will actually use.
func (h *Handler) buildRoutesView(flash, problem string) routesView {
	view := routesView{
		Owner:       h.owner,
		Page:        "routes",
		Flash:       hermesFlashMessage(flash),
		Error:       problem,
		HasRegistry: h.registry != nil,
		Rows:        []routeRowView{},
	}
	if h.settings != nil {
		view.Prefixes = h.settings.Strings(wording.PrefixesKey)
		view.Filler = h.settings.String(wording.FillerKey)
		view.Unavailable = h.settings.String(wording.UnavailableKey)
	}
	if h.registry == nil {
		return view
	}
	for _, name := range h.registry.Names() {
		info, ok := h.registry.Info(name)
		if !ok {
			continue
		}
		phrases, examples := 0, len(info.Examples)
		if h.settings != nil {
			phrases = len(h.settings.Strings(wording.ControlPhrasesKey(name)))
			if live := h.settings.Strings(wording.ExamplesKey(name)); len(live) > 0 {
				examples = len(live)
			}
		}
		row := routeRowView{
			Name:           name,
			Description:    info.Description,
			Permission:     string(info.Permission),
			FastPath:       info.HasSpeak || phrases > 0,
			AgentOnly:      info.AgentOnly,
			LocalOnly:      info.LocalOnly,
			Confirm:        info.Permission == tool.PermissionConfirm,
			Enabled:        info.Enabled,
			Hermes:         !info.LocalOnly && info.Permission != tool.PermissionConfirm,
			ControlPhrases: phrases,
			Examples:       examples,
		}
		view.Totals.Tools++
		if row.FastPath {
			view.Totals.FastPath++
		}
		if row.AgentOnly {
			view.Totals.AgentOnly++
		}
		if row.LocalOnly {
			view.Totals.LocalOnly++
		}
		if row.Confirm {
			view.Totals.Confirm++
		}
		if row.Hermes {
			view.Totals.Hermes++
		}
		view.Rows = append(view.Rows, row)
	}
	sort.Slice(view.Rows, func(i, j int) bool { return view.Rows[i].Name < view.Rows[j].Name })
	return view
}

// buildHermesView lists the MCP surface Hermes would get and every account.
func (h *Handler) buildHermesView(flash, problem string) hermesView {
	view := hermesView{
		Owner:           h.owner,
		Page:            "hermes",
		Flash:           hermesFlashMessage(flash),
		Error:           problem,
		HasRegistry:     h.registry != nil,
		HasIntegrations: h.integrations != nil,
		Surface:         []hermesToolView{},
		Rows:            []integrationView{},
	}
	if h.registry != nil {
		for _, name := range h.registry.Names() {
			info, ok := h.registry.Info(name)
			if !ok {
				continue
			}
			if info.LocalOnly {
				view.LocalOnlyCount++
				continue
			}
			if info.Permission == tool.PermissionConfirm {
				view.ConfirmCount++
				continue
			}
			view.Surface = append(view.Surface, hermesToolView{
				Name:        name,
				Description: info.Description,
				Permission:  string(info.Permission),
			})
		}
		view.SurfaceCount = len(view.Surface)
	}
	if h.integrations == nil {
		return view
	}
	for _, account := range h.integrations.List() {
		row := integrationView{
			ID:        account.ID,
			Name:      account.Name,
			Summary:   account.Summary,
			Docs:      account.Docs,
			Status:    integrationStatusLabel(account.Status),
			StatusKey: string(account.Status),
			Hermes:    account.Hermes,
			CanGrant:  account.CanGrant,
			Linkable:  account.Linkable,
			Tools:     account.Tools,
		}
		if row.Hermes {
			view.Granted++
		}
		for _, secret := range account.Secrets {
			row.Secrets = append(row.Secrets, integrationSecretView{
				Name:     secret.Name,
				Set:      secret.Set,
				Source:   secret.Source,
				Last4:    secret.Last4,
				Optional: secret.Optional,
			})
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

func integrationStatusLabel(status integrations.Status) string {
	switch status {
	case integrations.StatusLinked:
		return "Linked"
	case integrations.StatusEnvironment:
		return "From environment"
	case integrations.StatusPartial:
		return "Partly linked"
	default:
		return "Not linked"
	}
}

// integrationLinkMessage turns a validation error into the one sentence the
// page shows, without echoing a secret value.
func integrationLinkMessage(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "is required"):
		return "Fill every required field before saving."
	case strings.Contains(text, "does not belong"):
		return "The form contained an unknown field."
	default:
		return "The account could not be linked."
	}
}

// hermesFlashMessage maps one flash name to the page's label.
func hermesFlashMessage(name string) string {
	switch name {
	case flashHermesLinked:
		return "Account linked."
	case flashHermesUnlinked:
		return "Account unlinked; its secrets were cleared."
	case flashHermesGranted:
		return "Hermes access granted. It applies once Hermes is installed."
	case flashHermesRevoked:
		return "Hermes access revoked."
	default:
		return ""
	}
}
