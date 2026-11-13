package dashboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"gopkg.in/yaml.v3"
)

const owner = "alice@example.com"

// fakeLocation mirrors location.Service's health contract without a network
// lookup.
type fakeLocation struct {
	place location.Location
	err   error
}

func (f fakeLocation) Health(context.Context) error { return f.err }
func (f fakeLocation) Current() (location.Location, bool) {
	return f.place, f.place.Known()
}

type fakeModule string

func (m fakeModule) Name() string                             { return string(m) }
func (m fakeModule) Register(*tool.Registry, tool.Deps) error { return nil }

type fakeModules struct {
	names  []string
	health []tools.Health
	block  bool
}

func (f fakeModules) Modules() []tool.Module {
	modules := make([]tool.Module, 0, len(f.names))
	for _, name := range f.names {
		modules = append(modules, fakeModule(name))
	}
	return modules
}

func (f fakeModules) Health(ctx context.Context) []tools.Health {
	if f.block {
		<-ctx.Done()
		return nil
	}
	return f.health
}

// uiFixture is the small React build every test handler serves, so the app
// routes are deterministic without the real Vite bundle.
const uiIndex = "<!doctype html><title>fixture</title><script type=\"module\" src=\"/assets/app.js\"></script>"

func uiFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte(uiIndex)},
		"assets/app.js": {Data: []byte("console.log('fixture')")},
	}
}

// getJSON reads one /api/ui page as the owner and decodes the JSON body into
// v (when non-nil). It returns the recorder for status and header checks.
func getJSON(t *testing.T, handler http.Handler, target string, v any) *httptest.ResponseRecorder {
	t.Helper()
	rec := serve(handler, request(http.MethodGet, target, owner))
	decodeJSON(t, rec, v)
	return rec
}

// decodeJSON decodes a JSON response body, failing the test when it is not
// valid JSON.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if v == nil {
		return
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, status %d, body = %s", ct, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// noRawScript fails when a JSON body carries an unescaped script tag.
// encoding/json escapes <, > and & as \u00XX, so a hostile value cannot
// become markup if a client ever injects the body.
func noRawScript(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, "<script") || strings.Contains(body, "</script") {
		t.Errorf("response body contains an unescaped script tag:\n%s", body)
	}
}

func newHandler(t *testing.T, opts dashboard.Options) *dashboard.Handler {
	t.Helper()
	if opts.Owner == "" {
		opts.Owner = owner
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.ProxyAllowed == nil {
		opts.ProxyAllowed = func(ip net.IP) bool { return ip.IsLoopback() }
	}
	if opts.UI == nil {
		opts.UI = uiFixture()
	}
	if opts.Hosts == nil {
		// The tests' tailnet name; a loopback Host alone would not exercise the
		// configured-name path.
		opts.Hosts = []string{"pi.example.ts.net"}
	}
	handler, err := dashboard.New(opts)
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}
	return handler
}

func request(method, target, login string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	// A loopback Host is the documented local smoke test and the default the
	// handler accepts without extra configuration.
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	return req
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestNewDashboardRequiresAnOwner(t *testing.T) {
	if _, err := dashboard.New(dashboard.Options{}); err == nil {
		t.Fatal("New succeeded, want an owner error")
	}
}

func TestDashboardRejectsRequestsWithoutTheTailscaleLogin(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	for _, path := range []string{"/", "/activity", "/health", "/alerts", "/notes", "/settings", "/secrets", "/sounds", "/sounds/audio/chime", "/voices", "/voices/audio/alba", "/personas", "/spotify", "/assets/app.js", "/api/ui/session", "/api/ui/health", "/api/ui/activity", "/api/ui/settings", "/api/ui/secrets", "/api/ui/sounds", "/api/ui/voices", "/api/ui/personas", "/api/ui/alerts", "/api/ui/notes", "/api/ui/spotify", "/api/ui/volume"} {
		t.Run(path, func(t *testing.T) {
			rec := serve(handler, request(http.MethodGet, path, ""))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if body := rec.Body.String(); body != "forbidden\n" {
				t.Errorf("body = %q, want a uniform rejection", body)
			}
		})
	}
}

func TestDashboardRejectsADifferentTailscaleLogin(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/health", "mallory@example.com"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestDashboardRejectsADisplayNameWithoutTheLogin(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	req := request(http.MethodGet, "/health", "")
	req.Header.Set("Tailscale-User-Name", "Alice Architect")
	rec := serve(handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestDashboardRejectsAmbiguousIdentityHeaders(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	req := request(http.MethodGet, "/health", owner)
	req.Header.Add("Tailscale-User-Login", "mallory@example.com")
	rec := serve(handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestDashboardRejectsAnUntrustedSourceEvenWithTheRightIdentity(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	req := request(http.MethodGet, "/health", owner)
	req.RemoteAddr = "203.0.113.9:4444"
	// A caller-controlled forwarding header must not turn a remote source into
	// the loopback proxy.
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := serve(handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from a non-loopback source", rec.Code)
	}
}

func TestDashboardPassesOnlyTheValidatedLoginToHandlers(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	handler.Handle("/whoami", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, dashboard.Login(r.Context())+"|"+r.Header.Get("Tailscale-User-Name"))
	})
	req := request(http.MethodGet, "/whoami", owner)
	req.Header.Set("Tailscale-User-Name", "Alice Architect")
	rec := serve(handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); body != owner+"|" {
		t.Errorf("handler saw %q, want the login only", body)
	}
}

func TestDashboardServesTheHealthJSONToTheOwner(t *testing.T) {
	handler := newHandler(t, dashboard.Options{
		HealthTimeout: time.Second,
		Services: []dashboard.ServiceCheck{
			{Name: "embed", Check: func(context.Context) error { return nil }},
			{Name: "assistant", Check: func(context.Context) error { return nil }},
			{Name: "wakeword", Check: func(context.Context) error { return errors.New("dial tcp: connection refused") }},
			{Name: "stt", Check: func(context.Context) error { return nil }},
			{Name: "tts"},
		},
		Location: fakeLocation{place: location.Location{City: "Bristol", Region: "England", Timezone: "Europe/London"}},
		Modules: fakeModules{
			names: []string{"clock", "weather", "audio", "timer"},
			health: []tools.Health{
				{Module: "weather", Err: errors.New(`upstream <script>alert(1)</script> failed`)},
				{Module: "audio", Err: nil},
			},
		},
	})

	var view struct {
		Owner    string
		Page     string
		Services []struct{ Name, Status, StatusLabel, Detail string }
		Location struct{ Name, Status, StatusLabel, Detail string }
		Modules  []struct{ Name, Status, StatusLabel, Detail string }
		Spotify  []struct{ Name, Status, StatusLabel, Detail string }
	}
	rec := getJSON(t, handler, "/api/ui/health", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if view.Owner != owner || view.Page != "health" {
		t.Errorf("owner/page = %q/%q", view.Owner, view.Page)
	}
	services := map[string]string{}
	for _, svc := range view.Services {
		services[svc.Name] = svc.StatusLabel + "|" + svc.Detail
	}
	for name, want := range map[string]string{"embed": "", "wakeword": "connection refused", "tts": "not configured"} {
		got, ok := services[name]
		if !ok {
			t.Errorf("services lack %q: %v", name, services)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("service %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"assistant", "stt"} {
		if _, ok := services[name]; !ok {
			t.Errorf("services lack %q", name)
		}
	}
	body := rec.Body.String()
	for _, want := range []string{"timer", "no check", "Bristol, England (Europe/London)"} {
		if !strings.Contains(body, want) {
			t.Errorf("health JSON lacks %q", want)
		}
	}
	if len(view.Spotify) != 2 {
		t.Errorf("Spotify checks = %d, want pairing and Web API auth", len(view.Spotify))
	}
	// A hostile Tool module error round-trips exactly and cannot be raw markup.
	var weather string
	for _, module := range view.Modules {
		if module.Name == "weather" {
			weather = module.Detail
		}
	}
	if !strings.Contains(weather, `upstream <script>alert(1)</script> failed`) {
		t.Errorf("weather detail = %q, want the exact error text", weather)
	}
	noRawScript(t, body)
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", rec.Header().Get("Content-Security-Policy"))
	}
}

func TestDashboardHealthIsBoundedByItsTimeout(t *testing.T) {
	block := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	handler := newHandler(t, dashboard.Options{
		HealthTimeout: 25 * time.Millisecond,
		Services: []dashboard.ServiceCheck{
			{Name: "embed", Check: block},
			{Name: "assistant", Check: func(context.Context) error { return nil }},
		},
		Modules: fakeModules{names: []string{"weather"}, block: true},
	})

	start := time.Now()
	rec := serve(handler, request(http.MethodGet, "/api/ui/health", owner))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("Health read took %s, want the 25ms budget to bound it", elapsed)
	}
	body := rec.Body.String()
	if strings.Count(body, "timed out") < 2 {
		t.Errorf("health JSON lacks timed-out service and module rows:\n%s", body)
	}
}

func TestDashboardHealthRejectsOtherMethods(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodPost, "/api/ui/health", owner))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestDashboardUnknownPathIsNotFound(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/api/ui/nope", owner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDashboardMutatingRoutesRejectCrossOriginRequests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
		called bool
	}{
		{
			name:   "same-origin fetch metadata",
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
			want:   http.StatusNoContent,
			called: true,
		},
		{
			name: "matching origin",
			mutate: func(r *http.Request) {
				r.Host = "pi.example.ts.net"
				r.Header.Set("Origin", "https://pi.example.ts.net")
			},
			want:   http.StatusNoContent,
			called: true,
		},
		{
			name: "matching referer through a forwarded host",
			mutate: func(r *http.Request) {
				r.Host = "127.0.0.1:8082"
				r.Header.Set("X-Forwarded-Host", "pi.example.ts.net")
				r.Header.Set("Referer", "https://pi.example.ts.net/health")
			},
			want:   http.StatusNoContent,
			called: true,
		},
		{
			name: "cross-origin fetch metadata",
			mutate: func(r *http.Request) {
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				r.Header.Set("Origin", "https://evil.example")
			},
			want: http.StatusForbidden,
		},
		{
			name: "cross-origin origin",
			mutate: func(r *http.Request) {
				r.Host = "pi.example.ts.net"
				r.Header.Set("Origin", "https://evil.example")
			},
			want: http.StatusForbidden,
		},
		{
			name:   "missing origin",
			mutate: func(*http.Request) {},
			want:   http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newHandler(t, dashboard.Options{})
			called := false
			handler.Mutating("/mutate", func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			req := request(http.MethodPost, "/mutate", owner)
			req.Host = "127.0.0.1:8082"
			tt.mutate(req)
			rec := serve(handler, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if called != tt.called {
				t.Errorf("handler called = %v, want %v", called, tt.called)
			}
		})
	}
}

func TestDashboardAuthRunsBeforeTheMutationGuard(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	called := false
	handler.Mutating("/mutate", func(http.ResponseWriter, *http.Request) { called = true })
	req := request(http.MethodPost, "/mutate", "")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Error("mutation handler ran without an identity")
	}
}

func TestDashboardMountsTheAPIBehindAuthAndSameOrigin(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api "+r.Method+" "+r.URL.Path)
	})
	handler := newHandler(t, dashboard.Options{API: api})

	rec := serve(handler, request(http.MethodGet, "/api/settings", owner))
	if rec.Code != http.StatusOK || rec.Body.String() != "api GET /settings" {
		t.Fatalf("authenticated GET = %d %q", rec.Code, rec.Body.String())
	}

	rec = serve(handler, request(http.MethodGet, "/api/settings", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated API status = %d, want 403", rec.Code)
	}

	req := request(http.MethodPut, "/api/settings/timezone", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = serve(handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin PUT = %d", rec.Code)
	}

	rec = serve(handler, request(http.MethodPut, "/api/settings/timezone", owner))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin PUT = %d, want 403", rec.Code)
	}
}

func TestComposeServiceChecksCoverTheSevenServices(t *testing.T) {
	links := dashboard.ComposeServiceChecks(config.Default())
	names := make([]string, 0, len(links))
	for _, link := range links {
		names = append(names, link.Name)
	}
	want := []string{"embed", "assistant", "wakeword", "stt", "tts", "web-search", "librespot"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("services = %v, want %v", names, want)
	}
	if links[0].Check == nil {
		t.Error("embed check is not wired")
	}
	if links[1].Check == nil {
		t.Error("assistant check is not wired")
	}
	// The default config has no speech services, so those rows are explicitly
	// unconfigured rather than failures.
	for _, link := range links[2:5] {
		if link.Check != nil {
			t.Errorf("%s check = wired, want unconfigured", link.Name)
		}
	}
	if links[5].Check == nil {
		t.Error("web-search check is not wired")
	}
	// The default config has no librespot status volume, so the row is
	// explicitly unconfigured rather than a fabricated failure.
	if links[6].Check != nil {
		t.Error("librespot check = wired without a status file")
	}
}

func TestLibrespotServiceCheckReadsTheHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "librespot.status")
	cfg := config.Default()
	cfg.Spotify.PairingStatusFile = path
	links := dashboard.ComposeServiceChecks(cfg)
	var check dashboard.ServiceCheck
	for _, link := range links {
		if link.Name == "librespot" {
			check = link
		}
	}
	if check.Check == nil {
		t.Fatal("librespot check is not wired for a configured status file")
	}
	if err := check.Check(context.Background()); err == nil {
		t.Fatal("a missing heartbeat was reported healthy")
	}
	updated := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte("version=1\nstate=paired\nsession=unknown\ndevice=Clanka\nupdated="+updated+"\n"), 0o600); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	if err := check.Check(context.Background()); err != nil {
		t.Fatalf("fresh heartbeat check: %v", err)
	}
}

// TestWebSearchServiceCheckUsesTheModuleSection proves the sixth service check
// probes the same address tools.web registers, not a second hard-coded one.
func TestWebSearchServiceCheckUsesTheModuleSection(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
	}))
	defer server.Close()

	cfg := config.Default()
	if cfg.Tools.Sections == nil {
		cfg.Tools.Sections = map[string]yaml.Node{}
	}
	cfg.Tools.Sections["web"] = yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "base_url"},
		{Kind: yaml.ScalarNode, Value: server.URL},
	}}

	links := dashboard.ComposeServiceChecks(cfg)
	var check dashboard.ServiceCheck
	for _, link := range links {
		if link.Name == "web-search" {
			check = link
		}
	}
	if check.Check == nil {
		t.Fatal("web-search check is not wired for a configured base_url")
	}
	if err := check.Check(context.Background()); err != nil {
		t.Fatalf("web-search check: %v", err)
	}
	if path != "/healthz" {
		t.Fatalf("health path = %q", path)
	}
}

func TestHTTPServiceChecksTheStatus(t *testing.T) {
	var status int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()

	status = http.StatusOK
	if err := dashboard.HTTPService("embed", server.URL+"/healthz").Check(context.Background()); err != nil {
		t.Fatalf("healthy check: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := dashboard.HTTPService("embed", server.URL+"/healthz").Check(context.Background()); err == nil {
		t.Fatal("unhealthy check succeeded, want error")
	}
}
