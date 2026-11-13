package dashboard_test

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

func TestAppServesIndexForTheRootAndEveryClientRoute(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	for _, target := range []string{"/", "/alerts", "/health", "/settings", "/spotify", "/voice-samples", "/unknown/deep/route"} {
		t.Run(target, func(t *testing.T) {
			rec := serve(handler, request(http.MethodGet, target, owner))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != uiIndex {
				t.Errorf("body = %q, want the fixture index.html", got)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want HTML", ct)
			}
			csp := rec.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "form-action 'self'") || strings.Contains(csp, "accounts.spotify.com") {
				t.Errorf("document CSP = %q, want form-action 'self' with no external origin", csp)
			}
			// React Aria injects one stylesheet at runtime; it is allowed by
			// hash, never by 'unsafe-inline'.
			if !strings.Contains(csp, "style-src 'self' 'sha256-") {
				t.Errorf("document CSP = %q, want the React Aria stylesheet hash", csp)
			}
			if strings.Contains(csp, "unsafe-inline") {
				t.Errorf("document CSP = %q, want no unsafe-inline", csp)
			}
			if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self';") {
				t.Errorf("document CSP lost the base policy: %q", csp)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store for the identity-scoped document", got)
			}
		})
	}
}

func TestAppServesAssetsWithTheImmutablePrivateCache(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	rec := serve(handler, request(http.MethodGet, "/assets/app.js", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "console.log('fixture')" {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want JavaScript", ct)
	}
	// Assets are not documents, so they keep the strict form-action.
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.HasSuffix(csp, "form-action 'self'") {
		t.Errorf("asset CSP = %q, want form-action 'self'", csp)
	}
}

func TestAppUnknownAssetsAndAPIPathsAreRealNotFounds(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	for _, target := range []string{"/assets/missing.js", "/api/ui/nope", "/api/nothing"} {
		t.Run(target, func(t *testing.T) {
			rec := serve(handler, request(http.MethodGet, target, owner))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "fixture") {
				t.Error("a miss was answered with index.html")
			}
		})
	}
}

func TestAppRejectsNonGetMethods(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	req := request(http.MethodPost, "/", owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := serve(handler, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestAppWithoutABuildAnswersASetupMessage(t *testing.T) {
	handler := newHandler(t, dashboard.Options{UI: fstest.MapFS{"assets/app.js": {Data: []byte("x")}}})
	rec := serve(handler, request(http.MethodGet, "/", owner))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not built") {
		t.Errorf("body = %q, want a setup message", rec.Body.String())
	}
	// The API does not depend on the bundle.
	if rec := serve(handler, request(http.MethodGet, "/api/ui/session", owner)); rec.Code != http.StatusOK {
		t.Errorf("session without a build = %d, want 200", rec.Code)
	}
}

func TestAppStaysBehindIdentity(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	for _, target := range []string{"/", "/alerts", "/assets/app.js", "/assets/missing.js", "/api/ui/session"} {
		t.Run(target, func(t *testing.T) {
			rec := serve(handler, request(http.MethodGet, target, ""))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 without the Tailscale login", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "fixture") {
				t.Error("the bundle was served without an identity")
			}
		})
	}
}

func TestSessionReportsOwnerAndWiredServices(t *testing.T) {
	type session struct {
		Owner    string
		Typed    bool
		Mic      bool
		Volume   bool
		Activity bool
	}
	read := func(t *testing.T, opts dashboard.Options) session {
		t.Helper()
		var v session
		rec := getJSON(t, newHandler(t, opts), "/api/ui/session", &v)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return v
	}

	if v := read(t, dashboard.Options{}); v != (session{Owner: owner}) {
		t.Errorf("bare session = %+v, want only the owner", v)
	}
	typed := &micTyped{}
	if v := read(t, dashboard.Options{Typed: typed}); !v.Typed || v.Mic {
		t.Errorf("typed only = %+v, want Typed without Mic", v)
	}
	if v := read(t, dashboard.Options{Transcriber: &micTranscriber{}}); v.Typed || v.Mic {
		t.Errorf("transcriber only = %+v, want neither Typed nor Mic", v)
	}
	if v := read(t, dashboard.Options{Typed: typed, Transcriber: &micTranscriber{}}); !v.Typed || !v.Mic {
		t.Errorf("typed and transcriber = %+v, want Mic", v)
	}
	if v := read(t, dashboard.Options{Volume: &fakeVolume{}}); !v.Volume {
		t.Errorf("volume = %+v, want Volume", v)
	}
	if v := read(t, dashboard.Options{Activity: activityStoreForTest()}); !v.Activity {
		t.Errorf("activity = %+v, want Activity", v)
	}
}

// TestNoViewOffersAnOffOriginFormTarget keeps every write a same-origin target.
// The Spotify exception is gone (Connect is a GET link); this fails if a page
// starts pointing a form at another origin.
func TestNoViewOffersAnOffOriginFormTarget(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	pages := []string{"activity", "alerts", "health", "notes", "personas", "secrets", "settings", "sounds", "spotify", "voice-samples", "voices"}
	for _, page := range pages {
		var view any
		decodeJSON(t, serve(handler, request(http.MethodGet, "/api/ui/"+page, owner)), &view)
		for _, action := range formActions(view) {
			if !strings.HasPrefix(action, "/") || strings.HasPrefix(action, "//") {
				t.Errorf("%s offers an off-origin form target %q", page, action)
			}
		}
	}
}

// formActions collects every JSON field whose name ends in Action.
func formActions(v any) []string {
	var actions []string
	switch value := v.(type) {
	case map[string]any:
		for key, item := range value {
			if text, ok := item.(string); ok && strings.HasSuffix(key, "Action") && text != "" {
				actions = append(actions, text)
				continue
			}
			actions = append(actions, formActions(item)...)
		}
	case []any:
		for _, item := range value {
			actions = append(actions, formActions(item)...)
		}
	}
	return actions
}
