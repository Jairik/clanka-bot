package dashboard

import (
	"errors"
	"net/http"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// The OAuth binding cookie is separate from the state in the URL. It is
// SameSite=Lax so Spotify's top-level callback GET carries it, HttpOnly so no
// script can read it, and short-lived.
const (
	spotifyCookieName = "clanka_spotify_oauth"
	spotifyCookieTTL  = 10 * time.Minute
)

// Closed flash values. A callback can only select one of these; no query text
// is reflected into the page.
const (
	flashSpotifyConnected    = "spotify-connected"
	flashSpotifyDenied       = "spotify-denied"
	flashSpotifyInvalidState = "spotify-invalid-state"
	flashSpotifyExchange     = "spotify-exchange-failed"
	flashSpotifyNotReady     = "spotify-not-configured"
	flashSpotifyVerified     = "spotify-verified"
	flashSpotifyVerifyFailed = "spotify-verify-failed"
	flashSpotifyDisconnected = "spotify-disconnected"
	flashSpotifyError        = "spotify-error"
)

// spotifyView is the whole Spotify page. It contains metadata only: no access
// token, refresh token, authorization code, PKCE verifier or OAuth state.
type spotifyView struct {
	Owner   string
	Page    string
	Flash   string
	Error   string
	Pairing spotifyPairingView
	WebAPI  spotifyWebAPIView
	Scopes  []string
}

type spotifyPairingView struct {
	Configured bool
	Available  bool
	Paired     bool
	Pill       string
	Label      string
	Detail     string
	Device     string
	Updated    string
	UpdatedISO string
}

type spotifyWebAPIView struct {
	State              string
	Pill               string
	Label              string
	Detail             string
	ClientConfigured   bool
	RedirectConfigured bool
	RedirectURI        string
	Set                bool
	Source             string
	SourceLabel        string
	Updated            string
	UpdatedISO         string
	LastRefresh        string
	LastRefreshISO     string
	Expires            string
	ExpiresISO         string
	CanConnect         bool
	CanVerify          bool
	CanDisconnect      bool
}

// spotifyPage renders the separate Connect pairing and Web API auth cards.
func (h *Handler) spotifyPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildSpotifyView(spotifyFlashMessage(r.URL.Query().Get("flash")), ""))
}

// spotifyConnect starts the PKCE flow from a plain same-origin GET link. It is
// deliberately not a form POST: browsers apply form-action to a form's redirect
// chain, which would force accounts.spotify.com into the document policy. The
// state is generated server-side and validated on the callback, so a
// cross-site trigger can only start a flow the owner must still authorize. The
// response is a redirect to Spotify, never a rendered credential.
func (h *Handler) spotifyConnect(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if h.spotify == nil {
		http.NotFound(w, r)
		return
	}
	begin, err := h.spotify.Begin(Login(r.Context()))
	if err != nil {
		h.logger.Warn("dashboard spotify connect", "error", err.Error())
		h.spotifyRedirect(w, r, flashSpotifyNotReady)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     spotifyCookieName,
		Value:    begin.Binding,
		Path:     "/spotify",
		MaxAge:   int(spotifyCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.spotify.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, begin.AuthorizeURL, http.StatusSeeOther)
}

// spotifyCallback handles Spotify's top-level redirect. It is not wrapped in
// Mutating because the request comes from accounts.spotify.com. The one-use
// state, owner binding and binding cookie are the callback's forgery checks.
func (h *Handler) spotifyCallback(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if h.spotify == nil {
		http.NotFound(w, r)
		return
	}
	owner := Login(r.Context())
	state := r.URL.Query().Get("state")
	binding := ""
	if cookie, err := r.Cookie(spotifyCookieName); err == nil {
		binding = cookie.Value
	}
	h.clearSpotifyCookie(w)

	if callbackError := r.URL.Query().Get("error"); callbackError != "" {
		if err := h.spotify.ConsumeState(owner, state, binding); err != nil {
			h.logger.Warn("dashboard spotify callback", "result", "invalid-state")
			h.spotifyRedirect(w, r, flashSpotifyInvalidState)
			return
		}
		if callbackError == "access_denied" {
			h.spotifyRedirect(w, r, flashSpotifyDenied)
			return
		}
		h.spotifyRedirect(w, r, flashSpotifyError)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		h.spotifyRedirect(w, r, flashSpotifyInvalidState)
		return
	}
	err := h.spotify.Complete(r.Context(), owner, state, binding, code)
	switch {
	case err == nil:
		h.spotifyRedirect(w, r, flashSpotifyConnected)
	case errors.Is(err, spotify.ErrInvalidState):
		h.logger.Warn("dashboard spotify callback", "result", "invalid-state")
		h.spotifyRedirect(w, r, flashSpotifyInvalidState)
	case errors.Is(err, spotify.ErrNeedsReconnect), errors.Is(err, spotify.ErrUnavailable):
		h.logger.Warn("dashboard spotify callback", "result", "exchange-failed", "error", err.Error())
		h.spotifyRedirect(w, r, flashSpotifyExchange)
	case errors.Is(err, spotify.ErrNotConfigured), errors.Is(err, spotify.ErrStore):
		h.logger.Warn("dashboard spotify callback", "result", "not-configured", "error", err.Error())
		h.spotifyRedirect(w, r, flashSpotifyNotReady)
	default:
		h.logger.Warn("dashboard spotify callback", "result", "error", "error", err.Error())
		h.spotifyRedirect(w, r, flashSpotifyError)
	}
}

// spotifyVerify proves the stored refresh token still works without claiming
// a connection from mere metadata.
func (h *Handler) spotifyVerify(w http.ResponseWriter, r *http.Request) {
	if !allowPost(w, r) {
		return
	}
	if h.spotify == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.spotify.Refresh(r.Context()); err != nil {
		h.logger.Warn("dashboard spotify verify", "error", err.Error())
		h.spotifyRedirect(w, r, flashSpotifyVerifyFailed)
		return
	}
	h.spotifyRedirect(w, r, flashSpotifyVerified)
}

// spotifyDisconnect clears the Web API refresh token. Connect pairing is a
// different grant and is untouched.
func (h *Handler) spotifyDisconnect(w http.ResponseWriter, r *http.Request) {
	if !allowPost(w, r) {
		return
	}
	if h.spotify == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.spotify.Disconnect(r.Context()); err != nil {
		h.logger.Warn("dashboard spotify disconnect", "error", err.Error())
		h.spotifyRedirect(w, r, flashSpotifyError)
		return
	}
	h.spotifyRedirect(w, r, flashSpotifyDisconnected)
}

// spotifyRedirect uses a closed flash value and never reflects callback text.
func (h *Handler) spotifyRedirect(w http.ResponseWriter, r *http.Request, flash string) {
	http.Redirect(w, r, "/spotify?flash="+flash, http.StatusSeeOther)
}

// clearSpotifyCookie removes the binding cookie on every callback outcome.
func (h *Handler) clearSpotifyCookie(w http.ResponseWriter) {
	secure := true
	if h.spotify != nil {
		secure = h.spotify.SecureCookies()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     spotifyCookieName,
		Value:    "",
		Path:     "/spotify",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// buildSpotifyView reads the pairing heartbeat and Web API status separately.
// It never reads a credential value.
func (h *Handler) buildSpotifyView(flash, errorText string) spotifyView {
	view := spotifyView{Owner: h.owner, Page: "spotify", Flash: flash, Error: errorText, Scopes: append([]string(nil), spotify.Scopes...)}

	// Without a pairing reader the status is not merely unknown, it is not
	// wired; say so instead of sending an empty label and detail.
	pairing := spotify.PairingStatus{Label: "not configured", Detail: "Spotify Connect is not wired in this deployment."}
	if h.spotifyPairing != nil {
		pairing = h.spotifyPairing.Status()
	}
	device := pairing.Device
	if device == "" && h.spotify != nil {
		device = h.spotify.Config().DeviceName
	}
	if device == "" {
		device = spotify.DefaultDeviceName
	}
	view.Pairing = spotifyPairingView{
		Configured: pairing.Configured,
		Available:  pairing.Available,
		Paired:     pairing.Paired,
		Pill:       spotifyPill(pairing.State),
		Label:      pairing.Label,
		Detail:     pairing.Detail,
		Device:     device,
	}
	if !pairing.UpdatedAt.IsZero() {
		at := pairing.UpdatedAt.UTC()
		view.Pairing.Updated = at.Format("2 Jan 2006 15:04 MST")
		view.Pairing.UpdatedISO = at.Format(time.RFC3339)
	}

	webAPI := spotify.WebAPIStatus{
		State:  spotify.WebAPINotConfigured,
		Label:  "not configured",
		Detail: "Spotify is not wired in this deployment.",
	}
	if h.spotify != nil {
		webAPI = h.spotify.Status()
	}
	source := webAPI.Source
	if source == "" {
		source = string(secrets.SourceNone)
	}
	view.WebAPI = spotifyWebAPIView{
		State:              string(webAPI.State),
		Pill:               spotifyWebAPIPill(webAPI.State),
		Label:              webAPI.Label,
		Detail:             webAPI.Detail,
		ClientConfigured:   webAPI.ClientConfigured,
		RedirectConfigured: webAPI.RedirectConfigured,
		RedirectURI:        webAPI.RedirectURI,
		Set:                webAPI.Set,
		Source:             source,
		SourceLabel:        spotifySourceLabel(source),
		CanConnect:         webAPI.ClientConfigured && webAPI.RedirectConfigured,
		CanVerify:          webAPI.Set,
		CanDisconnect:      source == string(secrets.SourceStore),
	}
	if webAPI.HasUpdatedAt {
		at := webAPI.UpdatedAt.UTC()
		view.WebAPI.Updated = at.Format("2 Jan 2006 15:04 MST")
		view.WebAPI.UpdatedISO = at.Format(time.RFC3339)
	}
	if webAPI.HasLastRefresh {
		at := webAPI.LastRefresh.UTC()
		view.WebAPI.LastRefresh = at.Format("2 Jan 2006 15:04 MST")
		view.WebAPI.LastRefreshISO = at.Format(time.RFC3339)
	}
	if webAPI.HasExpiresAt {
		at := webAPI.ExpiresAt.UTC()
		view.WebAPI.Expires = at.Format("2 Jan 2006 15:04 MST")
		view.WebAPI.ExpiresISO = at.Format(time.RFC3339)
	}
	return view
}

// spotifyFlashMessage maps the closed set to user-visible text.
func spotifyFlashMessage(flash string) string {
	switch flash {
	case flashSpotifyConnected:
		return "Spotify Web API connected. The refresh token is stored in the write-only secrets table."
	case flashSpotifyDenied:
		return "Spotify authorization was cancelled. Nothing was stored."
	case flashSpotifyInvalidState:
		return "That sign-in link is no longer valid. Start the connection again."
	case flashSpotifyExchange:
		return "Spotify did not complete the sign-in. Nothing was stored; try again."
	case flashSpotifyNotReady:
		return "Spotify is not configured yet. Set spotify.client_id and spotify.redirect_uri, then restart."
	case flashSpotifyVerified:
		return "The stored refresh token worked; a new access token was issued."
	case flashSpotifyVerifyFailed:
		return "The stored refresh token could not be refreshed. Connect again."
	case flashSpotifyDisconnected:
		return "Stored refresh token cleared. A .env value still applies if one is set."
	case flashSpotifyError:
		return "The Spotify request failed. Nothing was changed."
	default:
		return ""
	}
}

// spotifySourceLabel keeps the two secret layers plain.
func spotifySourceLabel(source string) string {
	switch source {
	case string(secrets.SourceStore):
		return "stored"
	case string(secrets.SourceEnv):
		return ".env"
	default:
		return "none"
	}
}

// spotifyPill maps the pairing state to an existing CSS pill class.
func spotifyPill(state spotify.PairingState) string {
	switch state {
	case spotify.PairingPaired:
		return "ok"
	case spotify.PairingUnavailable, spotify.PairingExpired:
		return "error"
	default:
		return "not-configured"
	}
}

// spotifyWebAPIPill maps the Web API state to an existing CSS pill class.
func spotifyWebAPIPill(state spotify.WebAPIState) string {
	switch state {
	case spotify.WebAPIConnected:
		return "ok"
	case spotify.WebAPIReconnect, spotify.WebAPIError, spotify.WebAPIStoreUnavailable:
		return "error"
	default:
		return "not-configured"
	}
}

// allowPost is the method guard for the three same-origin Spotify actions.
func allowPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}
