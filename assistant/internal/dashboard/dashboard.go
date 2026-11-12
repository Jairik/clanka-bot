// Package dashboard serves the Tailscale-only web UI: server-rendered Go
// templates plus htmx, with no JavaScript build step.
package dashboard

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
)

// Modules is the Tool module list and its health checks.
type Modules = status.Modules

// Location is the place service the Health page reports.
type Location = status.Location

// ServiceCheck is one Compose service's health check. A nil Check means the
// service is not configured in this deployment, which is shown as such rather
// than as a failure. The type lives in internal/status so the page and the
// system.status Tool share one definition.
type ServiceCheck = status.ServiceCheck

// Options configures the Dashboard. Owner is the only Tailscale login allowed
// in; New refuses to build without one, so an unconfigured instance cannot
// accidentally serve unauthenticated pages.
type Options struct {
	Owner         string
	Logger        *slog.Logger
	HealthTimeout time.Duration
	Services      []ServiceCheck
	Modules       Modules
	Location      Location
	// Status optionally supplies the shared bounded snapshot main also gives
	// the system.status Tool. When nil, New builds one from the fields above.
	Status *status.Source
	// API is an optional unauthenticated handler mounted under /api/ behind
	// the Dashboard identity and same-origin checks. #40's settings handler is
	// mounted here; secrets and uploads can use the same seam.
	API http.Handler
	// Settings and ToolPolicy back the Settings page. They are the same
	// services the API and the voice Tools write, so the page needs no second
	// store or validation copy. The page is explanatory and read-only when
	// either is nil.
	Settings   *settings.Service
	ToolPolicy *toolpolicy.Policy
	// Secrets backs the write-only Secrets page: the same service the Agent
	// provider and Tool modules resolve values from. Only metadata is rendered.
	Secrets *secrets.Service
	// Sounds backs the Sounds page: the library, uploads and previews use the
	// same service the Cue roles read. PreviewQueue schedules a Pi preview
	// between turns; without one the handler plays synchronously.
	Sounds       *sound.Service
	PreviewQueue PreviewQueue
	// Voices, VoiceProbe and VoicePreview back the Voice page. The library is
	// the same service the voice Tools and startup rescan use; the probe is
	// required for an upload's header check and smoke test; the preview
	// synthesizes a bounded sample without touching the speaker or the active
	// setting. Without a TTS service the page is read-only.
	Voices       *voicelib.Service
	VoiceProbe   voicelib.Probe
	VoicePreview VoicePreview
	// ToolModules is the one wired Tool module list. The Scheduled alerts page
	// reaches the Timer, Alarm and Reminder managers through it instead of
	// adding three per-domain manager fields, so it cannot wire a second
	// registry that disagrees with the voice Tools.
	ToolModules ModuleProvider
	// Zone supplies the live timezone for displaying and cancelling alerts.
	// Nil means UTC; the page reads it per render, never snapshots it.
	Zone *tz.Service
	// Personas backs the Persona page: the same library and atomic selection
	// the persona Tools use. The page is read-only when it is nil.
	Personas *persona.Service
	// Spotify is the one Web API auth seam shared with the future Spotify
	// Tools. The page never renders a token; the pairing reader is separate
	// because Connect pairing and Web API auth are different grants.
	Spotify        *spotify.Auth
	SpotifyPairing *spotify.PairingReader
	// ProxyAllowed overrides the trusted source-address check. Production uses
	// the default (loopback plus the container's default gateway); tests inject
	// their own address policy.
	ProxyAllowed func(net.IP) bool
	// Now overrides the clock for deterministic page output in tests.
	Now func() time.Time
}

// Handler is the authenticated Dashboard mux. Register extra pages before
// serving; the auth and security middleware already wraps every route.
type Handler struct {
	owner          string
	logger         *slog.Logger
	healthStatus   *status.Source
	settings       *settings.Service
	tools          *toolpolicy.Policy
	secrets        *secrets.Service
	sounds         *sound.Service
	previews       PreviewQueue
	voices         *voicelib.Service
	voiceProbe     voicelib.Probe
	voicePreview   VoicePreview
	toolModules    ModuleProvider
	zone           *tz.Service
	now            func() time.Time
	personas       *persona.Service
	spotify        *spotify.Auth
	spotifyPairing *spotify.PairingReader
	proxyAllowed   func(net.IP) bool
	templates      *template.Template
	mux            *http.ServeMux
}

//go:embed templates/*.html static/*
var assets embed.FS

// New builds the Dashboard and registers its built-in routes. Every route,
// including static files, sits behind the identity check in ServeHTTP.
func New(opts Options) (*Handler, error) {
	owner := strings.TrimSpace(opts.Owner)
	if owner == "" {
		return nil, fmt.Errorf("dashboard: owner is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	templates, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse templates: %w", err)
	}
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("dashboard: static assets: %w", err)
	}

	// One shared bounded snapshot feeds the Health page; main passes the same
	// source to the system.status Tool. Tests may omit it and supply their own
	// checks directly.
	healthStatus := opts.Status
	if healthStatus == nil {
		healthStatus = status.New(status.Options{
			Timeout:  opts.HealthTimeout,
			Services: opts.Services,
			Modules:  opts.Modules,
			Location: opts.Location,
			Now:      now,
		})
	}
	h := &Handler{
		owner:          owner,
		logger:         logger,
		healthStatus:   healthStatus,
		settings:       opts.Settings,
		tools:          opts.ToolPolicy,
		secrets:        opts.Secrets,
		sounds:         opts.Sounds,
		previews:       opts.PreviewQueue,
		voices:         opts.Voices,
		voiceProbe:     opts.VoiceProbe,
		voicePreview:   opts.VoicePreview,
		toolModules:    opts.ToolModules,
		zone:           opts.Zone,
		now:            now,
		personas:       opts.Personas,
		spotify:        opts.Spotify,
		spotifyPairing: opts.SpotifyPairing,
		proxyAllowed:   opts.ProxyAllowed,
		templates:      templates,
		mux:            http.NewServeMux(),
	}
	// htmx is vendored under static/, so the UI works on a tailnet without
	// internet access.
	h.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	if opts.API != nil {
		// The API handler has no auth of its own; identity is enforced by
		// ServeHTTP and SameOrigin guards its mutating methods.
		h.mux.Handle("/api/", http.StripPrefix("/api", h.sameOrigin(opts.API)))
	}
	h.Handle("/health", h.healthPage)
	h.Handle("/health/panel", h.healthPanel)
	// Every Settings mutation goes through Mutating, so it inherits the
	// Dashboard identity and the same-origin guard.
	h.Handle("/settings", h.settingsPage)
	// The Secrets page lists every declared secret and offers replace/clear.
	// Both writes go through Mutating for identity and same-origin.
	h.Handle("/secrets", h.secretsPage)
	// The Sounds page owns uploads, role assignment, Ring settings and the two
	// previews. Every write goes through Mutating for identity and same-origin.
	h.Handle("/sounds", h.soundsPage)
	h.Handle("/sounds/audio/{name}", h.soundsAudio)
	h.Mutating("POST /sounds/upload", h.soundsUpload)
	h.Mutating("POST /sounds/roles/{role}", h.soundsRoleSave)
	h.Mutating("POST /sounds/roles/{role}/reset", h.soundsRoleReset)
	h.Mutating("POST /sounds/values/{key}", h.soundsValueSave)
	h.Mutating("POST /sounds/values/{key}/reset", h.soundsValueReset)
	h.Mutating("POST /sounds/preview/{name}", h.soundsPreview)
	// The Voice page owns upload, selection, rate and the optional source note.
	// Browser preview is an authenticated GET that synthesizes a bounded WAV
	// and never changes the persistent active voice.
	h.Handle("/voices", h.voicesPage)
	h.Handle("/voices/audio/{name}", h.voicesAudio)
	h.Mutating("POST /voices/upload", h.voicesUpload)
	h.Mutating("POST /voices/active/{name}", h.voicesActive)
	h.Mutating("POST /voices/rate", h.voicesRate)
	h.Mutating("POST /voices/rate/reset", h.voicesRateReset)
	h.Mutating("POST /voices/note/{name}", h.voicesNote)
	// The Persona page creates, edits and selects one bundle. Selection writes
	// voice, Filler, Unavailable wording and the active marker in one settings
	// transaction; every write goes through Mutating for identity and origin.
	h.Handle("/personas", h.personasPage)
	h.Mutating("POST /personas", h.personasCreate)
	h.Mutating("POST /personas/{name}", h.personasUpdate)
	h.Mutating("POST /personas/{name}/select", h.personasSelect)
	h.Mutating("POST /personas/active/reset", h.personasClear)
	h.Mutating("POST /settings/tools/{name}", h.settingsToolSave)
	h.Mutating("POST /settings/tools/{name}/reset", h.settingsToolReset)
	h.Mutating("POST /settings/values/{key}", h.settingsValueSave)
	h.Mutating("POST /settings/values/{key}/reset", h.settingsValueReset)
	// The Scheduled alerts page lists every active Timer, Alarm and Reminder,
	// cancels one by its manager id, and edits the per-Alarm Ring overrides.
	// Every write goes through Mutating for identity and same-origin.
	h.Handle("/alerts", h.alertsPage)
	h.Mutating("POST /alerts/{kind}/{id}/cancel", h.alertCancel)
	h.Mutating("POST /alerts/alarms/{id}", h.alertAlarmUpdate)
	h.Mutating("POST /alerts/alarms/{id}/reset", h.alertAlarmReset)
	h.Mutating("POST /secrets/{name}", h.secretsSet)
	h.Mutating("POST /secrets/{name}/clear", h.secretsClear)
	// Spotify Connect pairing and Web API auth are separate surfaces. The
	// callback is a cross-site top-level GET from accounts.spotify.com, so it
	// uses its own state/owner/cookie validation rather than Mutating.
	h.Handle("/spotify", h.spotifyPage)
	h.Handle("/spotify/callback", h.spotifyCallback)
	h.Mutating("/spotify/connect", h.spotifyConnect)
	h.Mutating("/spotify/verify", h.spotifyVerify)
	h.Mutating("/spotify/disconnect", h.spotifyDisconnect)
	h.Handle("/", h.index)
	return h, nil
}

// Handle registers an authenticated page or API route. The handler is
// responsible for its own method handling.
func (h *Handler) Handle(pattern string, handler http.HandlerFunc) {
	h.mux.HandleFunc(pattern, handler)
}

// Mutating registers a route that changes state. SameOrigin rejects requests a
// browser could have made from another origin; pair it with a same-site cookie
// when one is added later.
func (h *Handler) Mutating(pattern string, handler http.HandlerFunc) {
	h.mux.Handle(pattern, h.sameOrigin(http.HandlerFunc(handler)))
}

// sameOrigin wraps a handler whose methods may change state. It is the one
// CSRF policy for every write route, including the mounted API.
func (h *Handler) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP enforces the identity boundary before any route runs.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if !h.sourceAllowed(r.RemoteAddr) {
		h.reject(w, r, "untrusted source", "")
		return
	}
	login, ok := h.identity(r)
	if !ok {
		h.reject(w, r, "missing identity", "")
		return
	}
	if login != h.owner {
		h.reject(w, r, "unexpected identity", login)
		return
	}
	// Only the validated login may survive into a handler. Serve strips these
	// headers on the way in, but a direct caller can still set them, so the
	// application does not expose any other Tailscale header downstream.
	stripTailscaleHeaders(r.Header)
	ctx := context.WithValue(r.Context(), loginKey{}, login)
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !allowGet(w, r) {
		return
	}
	http.Redirect(w, r, "/health", http.StatusFound)
}

func (h *Handler) healthPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.render(w, "health_page", h.health(r.Context()))
}

func (h *Handler) healthPanel(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.render(w, "health_panel", h.health(r.Context()))
}

// render writes a parsed template. Templates are parsed at startup, so a
// runtime error here is unexpected and only logged.
func (h *Handler) render(w http.ResponseWriter, name string, view any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, view); err != nil {
		h.logger.Error("dashboard render", "template", name, "error", err.Error())
	}
}

func allowGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func setSecurityHeaders(w http.ResponseWriter) {
	// There is no inline script or style, so the page needs no unsafe CSP
	// exceptions. Nothing is cached because every page is identity-scoped.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
