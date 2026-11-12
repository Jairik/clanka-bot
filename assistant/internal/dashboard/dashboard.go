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

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/tools"
)

const defaultHealthTimeout = 3 * time.Second

// Modules is the Tool module list and its health checks.
type Modules interface {
	Modules() []tool.Module
	Health(context.Context) []tools.Health
}

// Location is the place service the Health page reports.
type Location interface {
	Health(context.Context) error
	Current() (location.Location, bool)
}

// ServiceCheck is one Compose service's health check. A nil Check means the
// service is not configured in this deployment, which is shown as such rather
// than as a failure.
type ServiceCheck struct {
	Name  string
	Check func(context.Context) error
}

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
	owner         string
	logger        *slog.Logger
	healthTimeout time.Duration
	services      []ServiceCheck
	modules       Modules
	location      Location
	settings      *settings.Service
	tools         *toolpolicy.Policy
	secrets       *secrets.Service
	sounds        *sound.Service
	previews      PreviewQueue
	proxyAllowed  func(net.IP) bool
	now           func() time.Time
	templates     *template.Template
	mux           *http.ServeMux
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
	timeout := opts.HealthTimeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
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

	h := &Handler{
		owner:         owner,
		logger:        logger,
		healthTimeout: timeout,
		services:      opts.Services,
		modules:       opts.Modules,
		location:      opts.Location,
		settings:      opts.Settings,
		tools:         opts.ToolPolicy,
		secrets:       opts.Secrets,
		sounds:        opts.Sounds,
		previews:      opts.PreviewQueue,
		proxyAllowed:  opts.ProxyAllowed,
		now:           now,
		templates:     templates,
		mux:           http.NewServeMux(),
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
	h.Mutating("POST /settings/tools/{name}", h.settingsToolSave)
	h.Mutating("POST /settings/tools/{name}/reset", h.settingsToolReset)
	h.Mutating("POST /settings/values/{key}", h.settingsValueSave)
	h.Mutating("POST /settings/values/{key}/reset", h.settingsValueReset)
	h.Mutating("POST /secrets/{name}", h.secretsSet)
	h.Mutating("POST /secrets/{name}/clear", h.secretsClear)
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
