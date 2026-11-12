package dashboard_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
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
	handler, err := dashboard.New(opts)
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}
	return handler
}

func request(method, target, login string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
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
	for _, path := range []string{"/", "/health", "/health/panel", "/alerts", "/settings", "/secrets", "/sounds", "/sounds/audio/chime", "/voices", "/voices/audio/alba", "/personas", "/static/app.css"} {
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

func TestDashboardServesTheHealthPageToTheOwner(t *testing.T) {
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

	rec := serve(handler, request(http.MethodGet, "/health", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Compose services", "embed", "assistant", "wakeword", "stt", "tts",
		"timer", "no check", "not configured", "connection refused",
		"Bristol, England (Europe/London)", "hx-get=\"/health/panel\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("Tool module error was not HTML-escaped")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("page does not contain the escaped error text")
	}
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

func TestDashboardHealthPanelIsAFragment(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/health/panel", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("panel response contains a full page")
	}
	if !strings.Contains(body, `id="health-panel"`) {
		t.Error("panel response lacks the refreshable section")
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
	rec := serve(handler, request(http.MethodGet, "/health", owner))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("Health page took %s, want the 25ms budget to bound it", elapsed)
	}
	body := rec.Body.String()
	if strings.Count(body, "timed out") < 2 {
		t.Errorf("page lacks timed-out service and module rows:\n%s", body)
	}
}

func TestDashboardRootRedirectsToHealth(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/", owner))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/health" {
		t.Errorf("Location = %q, want /health", got)
	}
}

func TestDashboardHealthRejectsOtherMethods(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodPost, "/health", owner))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestDashboardUnknownPathIsNotFound(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/nope", owner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDashboardServesVendoredHtmx(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/static/htmx.min.js", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "var htmx") {
		t.Error("body does not look like the vendored htmx build")
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

func TestComposeServiceChecksCoverTheFiveServices(t *testing.T) {
	links := dashboard.ComposeServiceChecks(config.Default())
	names := make([]string, 0, len(links))
	for _, link := range links {
		names = append(names, link.Name)
	}
	want := []string{"embed", "assistant", "wakeword", "stt", "tts"}
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
	for _, link := range links[2:] {
		if link.Check != nil {
			t.Errorf("%s check = wired, want unconfigured", link.Name)
		}
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
