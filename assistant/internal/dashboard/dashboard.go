// Package dashboard serves the Tailscale-only web UI: a React app built from
// assistant/web and embedded here, plus the JSON reads and form-encoded writes
// it calls. Every route sits behind the same identity and same-origin checks.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/httpguard"
	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/status"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wakesample"
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
	// VoiceSamples backs the wake-phrase recorder: takes on their own volume and
	// submitted bundles in the host-visible export directory. The page is
	// explanatory when it is nil.
	VoiceSamples *wakesample.Service
	// SampleRecorder taps the configured Pi microphone without starting an assistant turn.
	SampleRecorder SampleRecorder
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
	// Activity is the bounded recent-request store fed by the one latency
	// completion event. The page is read-only when it is nil.
	Activity *activity.Store
	// Typed answers a Dashboard typed request through the normal pipeline,
	// without speaking: main passes a reply.Session created with no speaker.
	// When nil the Activity page still lists requests but hides the box.
	Typed TypedResponder
	// Spoken answers a Dashboard request with Pi audio through the same reply
	// Session the voice loop uses. It is selected only while
	// dashboard.speak_replies is on; nil hides the top-bar toggle.
	Spoken TypedResponder
	// Transcriber turns a browser mic recording into text for Typed. The mic
	// button is hidden when it (or Typed) is nil.
	Transcriber Transcriber
	// Volume is the Pi speaker's level and mute for the top bar. The control
	// is hidden when it is nil.
	Volume VolumeControl
	// Wakeword is the configured wake-word allowlist and effective per-model
	// thresholds. Settings shows it read-only and Health checks it against the
	// service's advertised models. Zero means voice input is off.
	Wakeword WakewordInfo
	// Hosts is the explicit public Host allowlist: the tailnet name
	// `tailscale serve` exposes, exactly as it appears in the URL. Loopback
	// names are always accepted for the documented local smoke test. An empty
	// list keeps the Dashboard reachable only through a loopback Host, so a
	// DNS-rebinding page cannot borrow the owner identity; main logs the setup
	// instruction.
	Hosts []string
	// ProxyAllowed overrides the trusted source-address check. Production uses
	// the default (loopback plus the container's default gateway); tests inject
	// their own address policy.
	ProxyAllowed func(net.IP) bool
	// Now overrides the clock for deterministic page output in tests.
	Now func() time.Time
	// UI overrides the embedded React build (a directory holding index.html
	// and assets/). Tests inject a small fixture; production leaves it nil.
	UI fs.FS
	// UIURL is the internal base URL of the standalone Dashboard artifact (the
	// dashboard-web service). When set, the app tries it first and falls back
	// to the embedded bundle on any failure, so a UI-only deploy needs no Go
	// rebuild and a stopped service still serves a working page.
	UIURL string
	// UIVersion is the artifact pin reported in X-Dashboard-UI-Version.
	UIVersion string
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
	voiceSamples   *wakesample.Service
	sampleRecorder SampleRecorder
	toolModules    ModuleProvider
	zone           *tz.Service
	now            func() time.Time
	personas       *persona.Service
	spotify        *spotify.Auth
	spotifyPairing *spotify.PairingReader
	activity       *activity.Store
	typed          TypedResponder
	spoken         TypedResponder
	transcriber    Transcriber
	volume         VolumeControl
	// wakeword is the configured allowlist and thresholds shown on Settings
	// and probed on Health.
	wakeword WakewordInfo
	// live fans out the SSE stream: activity deltas plus sampled volume, alert
	// and health changes.
	live *liveHub
	// wakeModels caches the wakeword service's advertised models for the
	// voice-samples status marks (see voicesamples.go).
	wakeModels wakeModelCache
	// askLock serialises every typed request with a one-slot semaphore. The
	// voice loop is already single-turn; this gives the Dashboard the same
	// one-request-at-a-time behaviour and one pending Confirmation at a time.
	askLock chan struct{}
	// micLock admits one Dashboard mic upload at a time (see mic.go).
	micLock      chan struct{}
	hosts        httpguard.Hosts
	proxyAllowed func(net.IP) bool
	ui           fs.FS
	uiArtifact   *uiArtifact
	mux          *http.ServeMux
}

// uiBuild is the Vite output of assistant/web. The directory is committed with
// only a .gitkeep; `make web` (and the Docker build) fills ui/dist before the
// Go build, so a missing bundle shows a setup message instead of failing.
//
//go:embed all:ui
var uiBuild embed.FS

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
	hosts, err := httpguard.NewHosts(opts.Hosts)
	if err != nil {
		return nil, fmt.Errorf("dashboard: %w", err)
	}
	ui := opts.UI
	if ui == nil {
		ui, err = fs.Sub(uiBuild, "ui/dist")
		if err != nil {
			return nil, fmt.Errorf("dashboard: ui assets: %w", err)
		}
	}
	uiArtifact, err := newUIArtifact(opts.UIURL, opts.UIVersion, logger)
	if err != nil {
		return nil, err
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
		voiceSamples:   opts.VoiceSamples,
		sampleRecorder: opts.SampleRecorder,
		toolModules:    opts.ToolModules,
		zone:           opts.Zone,
		now:            now,
		personas:       opts.Personas,
		spotify:        opts.Spotify,
		spotifyPairing: opts.SpotifyPairing,
		activity:       opts.Activity,
		typed:          opts.Typed,
		spoken:         opts.Spoken,
		transcriber:    opts.Transcriber,
		volume:         opts.Volume,
		wakeword:       opts.Wakeword,
		askLock:        make(chan struct{}, 1),
		micLock:        make(chan struct{}, 1),
		hosts:          hosts,
		proxyAllowed:   opts.ProxyAllowed,
		ui:             ui,
		uiArtifact:     uiArtifact,
		mux:            http.NewServeMux(),
	}
	// The SSE hub is created before any route can be served; an Activity
	// observer pushes each completed request as one delta.
	h.live = newLiveHub(h)
	if h.activity != nil {
		h.activity.Subscribe(h.liveActivity)
	}
	if opts.API != nil {
		// The API handler has no auth of its own; identity is enforced by
		// ServeHTTP and SameOrigin guards its mutating methods.
		h.mux.Handle("/api/", http.StripPrefix("/api", h.sameOrigin(opts.API)))
	}
	// Each page's data is one JSON read under /api/ui/: the same view model
	// the server-rendered pages used, so labels and source rules stay in Go.
	// Writes keep their form-encoded routes below and answer with the
	// refreshed view (and a real status code on failure).
	h.Handle("GET /api/ui/session", h.sessionInfo)
	h.Handle("GET /api/ui/health", h.healthPage)
	h.Handle("GET /api/ui/activity", h.activityPage)
	// One read-only live stream for the whole app. Every write still uses its
	// own Mutating route; this only carries deltas.
	h.Handle("GET /api/ui/events", h.events)
	h.Handle("GET /api/ui/settings", h.settingsPage)
	h.Handle("GET /api/ui/secrets", h.secretsPage)
	h.Handle("GET /api/ui/sounds", h.soundsPage)
	h.Handle("GET /api/ui/voices", h.voicesPage)
	h.Handle("GET /api/ui/personas", h.personasPage)
	h.Handle("GET /api/ui/alerts", h.alertsPage)
	h.Handle("GET /api/ui/notes", h.notesPage)
	h.Handle("GET /api/ui/spotify", h.spotifyPage)
	// The wake-phrase recorder: reads the phrase view, adds or removes a
	// phrase, uploads one take as a raw WAV body, deletes one or all, and
	// submits the set as a bundle. Every
	// write goes through Mutating for identity and same-origin.
	h.Handle("GET /api/ui/voice-samples", h.voiceSamplesPage)
	h.Mutating("POST /voice-samples/phrases", h.voiceSamplesAddPhrase)
	h.Mutating("POST /voice-samples/phrases/{slug}/remove", h.voiceSamplesRemovePhrase)
	h.Mutating("POST /voice-samples/capture", h.voiceSamplesCapture)
	h.Mutating("POST /voice-samples/takes/{slug}/{kind}", h.voiceSamplesUpload)
	h.Mutating("POST /voice-samples/takes/{slug}/{kind}/{id}/delete", h.voiceSamplesDelete)
	h.Mutating("POST /voice-samples/clear/{slug}/{kind}", h.voiceSamplesClear)
	h.Mutating("POST /voice-samples/submit/{slug}", h.voiceSamplesSubmit)
	h.Handle("/voice-samples/take/{slug}/{kind}/{id}", h.voiceSamplesAudio)
	h.Handle("/voice-samples/bundle/{slug}/{name}", h.voiceSamplesBundle)
	// The typed-request box. The one write goes through Mutating for identity
	// and same-origin.
	h.Mutating("POST /activity/ask", h.activityAsk)
	h.Mutating("POST /api/ui/ask/audio", h.activityAskAudio)
	// The top bar's speaker volume and mute.
	// BLE routes share existing identity and same-origin mutation checks.
	h.Handle("GET /api/ui/lights", h.lightsAPI)
	h.Mutating("POST /api/ui/lights", h.lightsAPI)
	h.Handle("GET /api/ui/volume", h.volumeGet)
	h.Mutating("POST /api/ui/volume", h.volumeSet)
	// The Sounds page owns uploads, role assignment, Ring settings and the two
	// previews. Every write goes through Mutating for identity and same-origin.
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
	h.Handle("/voices/audio/{name}", h.voicesAudio)
	h.Mutating("POST /voices/upload", h.voicesUpload)
	h.Mutating("POST /voices/active/{name}", h.voicesActive)
	h.Mutating("POST /voices/rate", h.voicesRate)
	h.Mutating("POST /voices/rate/reset", h.voicesRateReset)
	h.Mutating("POST /voices/note/{name}", h.voicesNote)
	// The Persona page creates, edits and selects one bundle. Selection writes
	// voice, Filler, Unavailable wording and the active marker in one settings
	// transaction; every write goes through Mutating for identity and origin.
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
	h.Mutating("POST /alerts/{kind}/{id}/cancel", h.alertCancel)
	h.Mutating("POST /alerts/alarms/{id}", h.alertAlarmUpdate)
	h.Mutating("POST /alerts/alarms/{id}/reset", h.alertAlarmReset)
	// The Notes page owns the two fixed Lists and every Note. Every write goes
	// through Mutating for identity and same-origin.
	h.Mutating("POST /notes", h.noteCreate)
	h.Mutating("POST /notes/{id}", h.noteUpdate)
	h.Mutating("POST /notes/{id}/delete", h.noteDelete)
	h.Mutating("POST /notes/lists/{list}/items", h.listItemAdd)
	h.Mutating("POST /notes/lists/{list}/items/{id}", h.listItemUpdate)
	h.Mutating("POST /notes/lists/{list}/items/{id}/toggle", h.listItemToggle)
	h.Mutating("POST /notes/lists/{list}/items/{id}/remove", h.listItemRemove)
	h.Mutating("POST /secrets/{name}", h.secretsSet)
	h.Mutating("POST /secrets/{name}/clear", h.secretsClear)
	// The top-bar opt-in is a same-origin write like every other setting.
	h.Mutating("POST /api/ui/speak", h.speakToggle)
	// Spotify Connect pairing and Web API auth are separate surfaces. The
	// callback is a cross-site top-level GET from accounts.spotify.com, so it
	// uses its own state/owner/cookie validation rather than Mutating. Connect
	// is a plain GET link: a form POST would make the browser apply
	// form-action to Spotify's redirect, forcing its origin into the policy.
	h.Handle("GET /spotify/callback", h.spotifyCallback)
	h.Handle("GET /spotify/connect", h.spotifyConnect)
	h.Mutating("/spotify/verify", h.spotifyVerify)
	h.Mutating("/spotify/disconnect", h.spotifyDisconnect)
	// Everything else is the React app: hashed assets, or index.html for any
	// page path so a reload or a bookmarked /alerts opens the right page.
	h.Handle("/", h.app)
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
	// Reject a Host outside the configured tailnet name before reading any
	// identity header, so a DNS-rebinding page cannot borrow the owner login it
	// has guessed and a local process cannot use an arbitrary name.
	if !h.hosts.Allows(r) {
		h.reject(w, r, "unexpected host", "")
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
	// One central gzip for the document and the JSON views. A HEAD, a Range
	// request, a precompressed asset (which sets Content-Encoding itself) and
	// the SSE stream (which must flush per frame) pass through untouched.
	if r.Method != http.MethodHead && r.Header.Get("Range") == "" && r.URL.Path != "/api/ui/events" && acceptsEncoding(r.Header.Get("Accept-Encoding"), "gzip") {
		cw := &compressWriter{ResponseWriter: w}
		defer func() { _ = cw.Close() }()
		h.mux.ServeHTTP(cw, r.WithContext(ctx))
		return
	}
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) healthPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.health(r.Context()))
}

// sessionView is what the app shell needs before any page loads: who is signed
// in and which optional services are wired, so it can hide dead controls.
type sessionView struct {
	Owner    string
	Typed    bool
	Mic      bool
	Volume   bool
	Activity bool
	CanSpeak bool
	Speak    bool
	// APIVersion is the view-shape contract this server serves. A UI built
	// against a different value refuses to render instead of showing undefined
	// fields; see internal/dashboard/version.go.
	APIVersion int
}

func (h *Handler) sessionInfo(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, sessionView{
		Owner:      h.owner,
		Typed:      h.typed != nil,
		Mic:        h.typed != nil && h.transcriber != nil,
		Volume:     h.volume != nil,
		Activity:   h.activity != nil,
		CanSpeak:   h.spoken != nil && h.settings != nil,
		Speak:      h.speaking(),
		APIVersion: APIVersion,
	})
}

// writeJSON encodes one view. Views are plain data built from bounded
// snapshots, so an encode error is unexpected and only logged.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, view any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Every view carries the contract it was built from, so a UI can fail
	// loudly even when it does not read the session first.
	w.Header().Set("X-Dashboard-API-Version", strconv.Itoa(APIVersion))
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(view); err != nil {
		h.logger.Error("dashboard json", "error", err.Error())
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

// contentSecurityPolicy is the Dashboard policy with the given form-action and
// style-src sources. JSON and assets use 'self' for both; the app document adds
// Spotify's accounts origin and the one hashed stylesheet below.
func contentSecurityPolicy(formAction, styleSrc string) string {
	return "default-src 'self'; script-src 'self'; style-src " + styleSrc + "; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action " + formAction
}

// reactAriaStyleHash is the exact stylesheet React Aria injects at runtime
// (its pressable touch-action rule). It cannot be a static file, so the
// document allows that one sheet by hash instead of 'unsafe-inline'. The
// browser smoke test fails when a dependency upgrade changes the bytes; the
// new hash is the one the browser reports.
const reactAriaStyleHash = "'sha256-38RhXrc7EdReTKsOm23ZPOCUgniTUUcjky8QOOrQx6o='"

// appSecurityPolicy is the one document policy. The app is a single document
// for every page, so media-src is document-wide rather than page-scoped; it
// adds only blob: for a just-recorded take and 'self' for a kept one.
func appSecurityPolicy() string {
	return contentSecurityPolicy("'self'", "'self' "+reactAriaStyleHash) + "; media-src 'self' blob:"
}

func setSecurityHeaders(w http.ResponseWriter) {
	// The built app has no inline script, and its only runtime-injected style
	// is React Aria's hashed sheet, so no 'unsafe-inline' exception is needed.
	// Nothing is cached because every page is identity-scoped.
	w.Header().Set("Cache-Control", "no-store")
	// Every dashboard response has a compressed and a plain form, even when
	// this client accepts neither.
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'self'", "'self'"))
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
