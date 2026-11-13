package dashboard_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

// artifactServer is the standalone dashboard-web service, in miniature: a
// document, one hashed asset and its brotli sibling.
func artifactServer(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"/index.html":       "<!doctype html><title>artifact build</title>",
		"/assets/app.js":    "console.log('artifact asset')",
		"/assets/app.js.br": "artifact-brotli-bytes",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			http.Error(w, "broken", http.StatusInternalServerError)
		default:
			body, ok := files[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			// The artifact reports its own build, as nginx does from the image
			// entrypoint template.
			w.Header().Set("X-Dashboard-UI-Version", "real-sha")
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// artifactHandler serves the fixture as the embedded fallback and the test
// server as the standalone artifact.
func artifactHandler(t *testing.T, url string) *dashboard.Handler {
	t.Helper()
	return newHandler(t, dashboard.Options{UIURL: url, UIVersion: "abc123"})
}

func TestUIDocumentComesFromTheStandaloneArtifact(t *testing.T) {
	server := artifactServer(t)
	handler := artifactHandler(t, server.URL)

	rec := serve(handler, request(http.MethodGet, "/activity", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "artifact build") {
		t.Fatalf("document did not come from the artifact: %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-Dashboard-UI"); got != "artifact" {
		t.Errorf("X-Dashboard-UI = %q, want artifact", got)
	}
	if got := rec.Header().Get("X-Dashboard-UI-Version"); got != "real-sha" {
		t.Errorf("X-Dashboard-UI-Version = %q, want the artifact's own build", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("artifact document lost the CSP: %q", csp)
	}
}

func TestUIAssetPrefersTheArtifactBrotliSibling(t *testing.T) {
	server := artifactServer(t)
	handler := artifactHandler(t, server.URL)

	req := request(http.MethodGet, "/assets/app.js", owner)
	req.Header.Set("Accept-Encoding", "br")
	rec := serve(handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if got := rec.Body.String(); got != "artifact-brotli-bytes" {
		t.Fatalf("body = %q, want the artifact sibling", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("hashed asset lost its cache header: %q", got)
	}
}

func TestUIArtifactStoppedFallsBackToTheEmbeddedBundle(t *testing.T) {
	server := artifactServer(t)
	url := server.URL
	// A stopped service is a connection error; a started-but-broken one is a
	// non-200. Both must serve the embedded copy.
	server.Close()

	handler := artifactHandler(t, url)
	rec := serve(handler, request(http.MethodGet, "/", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fixture") {
		t.Fatalf("document = %s, want the embedded fixture", rec.Body.String())
	}
	if got := rec.Header().Get("X-Dashboard-UI"); got != "embedded" {
		t.Errorf("X-Dashboard-UI = %q, want embedded", got)
	}

	// An asset the artifact no longer has also falls back, which is what an
	// old tab needs during a rolling deploy.
	req := request(http.MethodGet, "/assets/app.js", owner)
	req.Header.Set("Accept-Encoding", "br")
	asset := serve(handler, req)
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "fixture") {
		t.Fatalf("asset fallback = %d %q", asset.Code, asset.Body.String())
	}
}

func TestUIArtifactMissingAssetIsStillA404(t *testing.T) {
	server := artifactServer(t)
	handler := artifactHandler(t, server.URL)

	rec := serve(handler, request(http.MethodGet, "/assets/nope.js", owner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when neither copy has the asset", rec.Code)
	}
}

func TestUIURLMustBeAnInternalBaseURL(t *testing.T) {
	for _, raw := range []string{"ftp://dashboard-web", "http://user:pass@dashboard-web", "http://dashboard-web/?x=1", "not a url"} {
		if _, err := dashboard.New(dashboard.Options{Owner: owner, UIURL: raw}); err == nil {
			t.Errorf("UIURL %q was accepted", raw)
		}
	}
}

func TestEveryJSONViewCarriesTheAPIVersion(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := getJSON(t, handler, "/api/ui/activity", nil)
	if got := rec.Header().Get("X-Dashboard-API-Version"); got != fmt.Sprint(dashboard.APIVersion) {
		t.Fatalf("X-Dashboard-API-Version = %q, want %d", got, dashboard.APIVersion)
	}
}

// TestAPIVersionMatchesTheUI pins the two artifacts to one contract: a Go bump
// must change web/src/api-version.ts in the same commit or the UI would refuse
// to render against the server it ships with.
func TestAPIVersionMatchesTheUI(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/api-version.ts")
	if err != nil {
		t.Fatalf("read the UI version: %v", err)
	}
	want := fmt.Sprintf("API_VERSION = %d;", dashboard.APIVersion)
	if !strings.Contains(string(raw), want) {
		t.Fatalf("web/src/api-version.ts must contain %q; it says:\n%s", want, raw)
	}
}

// TestUIArtifactVersionFallsBackToTheConfiguredPin covers an artifact that does
// not report a build (an older image, or a proxy in front of it).
func TestUIArtifactVersionFallsBackToTheConfiguredPin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "<!doctype html><title>artifact build</title>")
	}))
	t.Cleanup(server.Close)
	handler := artifactHandler(t, server.URL)

	rec := serve(handler, request(http.MethodGet, "/", owner))
	if got := rec.Header().Get("X-Dashboard-UI-Version"); got != "abc123" {
		t.Fatalf("X-Dashboard-UI-Version = %q, want the configured pin", got)
	}
}
