package dashboard

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
)

// Flash values are a small closed set, so a redirect can never reflect
// arbitrary text into the page.
const (
	flashSecretSaved       = "secret-saved"
	flashSecretClearedEnv  = "secret-cleared-env"
	flashSecretClearedNone = "secret-cleared-none"
)

// maxSecretFormBytes bounds the write-only form body; a secret is a short
// token, and the limit keeps a hostile body from being buffered.
const maxSecretFormBytes = 64 << 10

// secretsView is the write-only Secrets page. Values never enter this model;
// it is built from the service's metadata only.
type secretsView struct {
	Owner      string
	Page       string
	Flash      string
	Error      string
	HasSecrets bool
	Secrets    []secretView
}

// secretView is one declared secret's safe summary and its two write actions.
type secretView struct {
	Name        string
	Anchor      string
	Set         bool
	SetLabel    string
	Source      string
	SourceLabel string
	Last4       string
	Updated     string
	UpdatedISO  string
	Stored      bool
	SetAction   string
	ClearAction string
	Target      string
}

// secretsPage renders every secret the service declares, in declaration order.
func (h *Handler) secretsPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildSecretsView(secretsFlashMessage(r.URL.Query().Get("flash")), ""))
}

// secretsSet stores one replacement value. The submitted value is handed
// straight to the service and never enters the view, a log line or a redirect.
func (h *Handler) secretsSet(w http.ResponseWriter, r *http.Request) {
	if !h.secretsReady(w, r) {
		return
	}
	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretFormBytes)
	if err := r.ParseForm(); err != nil {
		h.secretsFail(w, r, http.StatusBadRequest, "Could not read the form.")
		return
	}
	if _, err := h.secrets.Set(r.Context(), name, r.FormValue("value")); err != nil {
		h.secretsError(w, r, "set", name, "The secret could not be stored.", err)
		return
	}
	h.secretsSuccess(w, r, flashSecretSaved)
}

// secretsClear removes one stored value. The service returns the metadata
// after the delete, so the page can say whether .env took over or the secret
// became unset.
func (h *Handler) secretsClear(w http.ResponseWriter, r *http.Request) {
	if !h.secretsReady(w, r) {
		return
	}
	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretFormBytes)
	if err := r.ParseForm(); err != nil {
		h.secretsFail(w, r, http.StatusBadRequest, "Could not read the form.")
		return
	}
	meta, err := h.secrets.Clear(r.Context(), name)
	if err != nil {
		h.secretsError(w, r, "clear", name, "The secret could not be cleared.", err)
		return
	}
	flash := flashSecretClearedNone
	if meta.Source == secrets.SourceEnv {
		flash = flashSecretClearedEnv
	}
	h.secretsSuccess(w, r, flash)
}

// secretsReady guards the write routes against an unwired service.
func (h *Handler) secretsReady(w http.ResponseWriter, r *http.Request) bool {
	if h.secrets == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

// secretsSuccess answers htmx with the swapped content and a plain browser
// form with a redirect, so both journeys work without JavaScript.
func (h *Handler) secretsSuccess(w http.ResponseWriter, r *http.Request, flash string) {
	h.writeJSON(w, http.StatusOK, h.buildSecretsView(secretsFlashMessage(flash), ""))
}

// secretsFail re-renders only metadata and a fixed message. Rejected input is
// deliberately not preserved: echoing a secret would defeat write-only.
func (h *Handler) secretsFail(w http.ResponseWriter, r *http.Request, status int, message string) {
	view := h.buildSecretsView("", message)
	h.writeJSON(w, status, view)
}

// secretsError maps a service error to a safe status and message, then logs
// the operation and name only. The value is never part of either.
func (h *Handler) secretsError(w http.ResponseWriter, r *http.Request, action, name, fallback string, err error) {
	status, message := http.StatusInternalServerError, fallback
	switch {
	case errors.Is(err, secrets.ErrUnknownSecret):
		status, message = http.StatusNotFound, "Unknown secret."
	case errors.Is(err, secrets.ErrEmptyValue):
		status, message = http.StatusBadRequest, "Enter a value."
	}
	h.logger.Warn("dashboard secrets",
		"action", action,
		"name", name,
		"status", status,
		"error", err.Error(),
	)
	h.secretsFail(w, r, status, message)
}

// secretsFlashMessage maps the closed flash set to the user-visible result.
func secretsFlashMessage(flash string) string {
	switch flash {
	case flashSecretSaved:
		return "Stored. The new value applies on the next request."
	case flashSecretClearedEnv:
		return "Stored value cleared. The .env value applies again."
	case flashSecretClearedNone:
		return "Stored value cleared. The secret is now unset."
	default:
		return ""
	}
}

// buildSecretsView reads one service snapshot. A future module declaration
// appears automatically because the list comes from the service, not a page
// constant.
func (h *Handler) buildSecretsView(flash, errorText string) secretsView {
	view := secretsView{Owner: h.owner, Page: "secrets", Flash: flash, Error: errorText}
	if h.secrets == nil {
		return view
	}
	view.HasSecrets = true
	for _, meta := range h.secrets.List() {
		view.Secrets = append(view.Secrets, newSecretView(meta))
	}
	return view
}

func newSecretView(meta secrets.Metadata) secretView {
	view := secretView{
		Name:        meta.Name,
		Anchor:      "secret-" + meta.Name,
		Set:         meta.Set,
		SetLabel:    "not set",
		Source:      string(meta.Source),
		SourceLabel: secretSourceLabel(meta.Source),
		Last4:       meta.Last4,
		Stored:      meta.Source == secrets.SourceStore,
		SetAction:   "/secrets/" + url.PathEscape(meta.Name),
		ClearAction: "/secrets/" + url.PathEscape(meta.Name) + "/clear",
		Target:      "#secrets-content",
	}
	if meta.Set {
		view.SetLabel = "set"
	}
	if meta.UpdatedAt != nil {
		// Render in UTC so the timestamp always names its zone; the database
		// stores UTC and no local zone is guessed.
		at := meta.UpdatedAt.UTC()
		view.Updated = at.Format("2 Jan 2006 15:04 MST")
		view.UpdatedISO = at.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

// secretSourceLabel keeps the three layers plain: a stored value, the .env
// bootstrap, or neither.
func secretSourceLabel(source secrets.Source) string {
	switch source {
	case secrets.SourceStore:
		return "stored"
	case secrets.SourceEnv:
		return ".env"
	default:
		return "none"
	}
}
