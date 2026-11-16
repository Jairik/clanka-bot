package dashboard_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

// methodTokens are the HTTP methods the route table may carry as a leading
// pattern token. A pattern without one of these is method-less (matches any
// method), which is the footgun the guard-invariant tests target.
var methodTokens = map[string]bool{
	"GET":     true,
	"POST":    true,
	"PUT":     true,
	"PATCH":   true,
	"DELETE":  true,
	"HEAD":    true,
	"OPTIONS": true,
}

// routeMethod returns the leading method token of a registration pattern and
// whether the pattern names a method at all. Method-less patterns (such as
// "/sounds/audio/{name}") return ("", false).
func routeMethod(pattern string) (string, bool) {
	idx := strings.IndexByte(pattern, ' ')
	if idx < 0 {
		return "", false
	}
	m := pattern[:idx]
	if !methodTokens[m] {
		return "", false
	}
	return m, true
}

// allowGetRoutes documents every route New registers WITHOUT the same-origin
// guard. Each entry is an authenticated, read-only GET (the handler calls
// allowGet) or a deliberate cross-site Spotify redirect, proven to never change
// state. The invariant tests fail if an unguarded route appears that is not
// justified here, and if an entry goes stale (its route is no longer
// registered) — which is exactly what would happen if a reviewer "fixed" the
// Spotify callback into Mutating.
var allowGetRoutes = map[string]string{
	// Authenticated GETs that stream bytes and never mutate.
	"/sounds/audio/{name}":                "authenticated GET streams one stored sound file; never mutates",
	"/voices/audio/{name}":                "authenticated GET synthesizes a bounded WAV and never changes the active voice",
	"/voice-samples/take/{slug}/{kind}/{id}": "authenticated GET serves one recorded take; never mutates",
	"/voice-samples/bundle/{slug}/{name}": "authenticated GET serves a submitted bundle export; never mutates",
	"/": "the React app document, served read-only",
	// Deliberate cross-site exceptions: a browser reaches these from
	// accounts.spotify.com, so the same-origin policy cannot apply.
	"GET /spotify/callback": "cross-site top-level GET from Spotify: one-use state plus a binding cookie; validated, not same-origin",
	"GET /spotify/connect":  "starts the OAuth flow; a form POST would force Spotify's origin into the form-action policy",
}

// TestDashboardEveryWriteRouteIsGuarded proves the common mistake fails: a
// state-changing method registered through Handle (the shorter, more obvious
// name) instead of Mutating would ship without CSRF protection. Every pattern
// that explicitly names POST, PUT, PATCH or DELETE must be guarded.
func TestDashboardEveryWriteRouteIsGuarded(t *testing.T) {
	h := newHandler(t, dashboard.Options{})
	for _, r := range h.Routes() {
		method, has := routeMethod(r.Pattern)
		if !has {
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if !r.Guarded {
				t.Errorf("route %q uses %s but is not guarded by Mutating", r.Pattern, method)
			}
		}
	}
}

// TestDashboardReadOnlyRoutesAreAllowlisted proves two things: every method-less
// registration (the routes that call allowGet) is either guarded or documented
// with a one-line reason, and the allowlist carries no stale entries. A future
// reviewer moving the Spotify callback into Mutating would leave a stale entry
// and break this test.
func TestDashboardReadOnlyRoutesAreAllowlisted(t *testing.T) {
	h := newHandler(t, dashboard.Options{})
	registered := make(map[string]bool, len(h.Routes()))
	for _, r := range h.Routes() {
		registered[r.Pattern] = true
		// A method-less registration that is not guarded must be justified.
		if _, hasMethod := routeMethod(r.Pattern); !hasMethod && !r.Guarded {
			reason, ok := allowGetRoutes[r.Pattern]
			if !ok {
				t.Errorf("method-less unguarded route %q has no allowlist entry; every non-Mutating method-less route must be justified", r.Pattern)
				continue
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("allowlist entry for %q is missing its one-line reason", r.Pattern)
			}
		}
	}
	// No stale allowlist entries: every documented exception must be a real,
	// currently registered route.
	for pattern, reason := range allowGetRoutes {
		if !registered[pattern] {
			t.Errorf("allowlist entry %q is not a registered route (stale?): %s", pattern, reason)
		}
	}
}

// TestDashboardWriteRoutesRejectCrossSiteBeforeHandler posts cross-site
// requests to a handful of real write URLs with the services unwired and asserts
// the same-origin guard returns 403 before any handler runs, so a missing guard
// could not reach application code.
func TestDashboardWriteRoutesRejectCrossSiteBeforeHandler(t *testing.T) {
	h := newHandler(t, dashboard.Options{})
	writeURLs := []string{
		"/activity/ask",
		"/api/ui/ask/audio",
		"/sounds/upload",
		"/voices/active/alba",
		"/personas",
		"/notes",
		"/settings/values/theme/reset",
		"/api/ui/volume",
		"/alerts/create",
		"/hermes/integrations/spotify/verify",
	}
	for _, target := range writeURLs {
		req := request(http.MethodPost, target, owner)
		// Sec-Fetch-Site: cross-site is what a malicious origin's browser fetch
		// carries; the owner header is still present (identity passes), so a 403
		// here proves the guard, not the auth check, rejected it.
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := serve(h, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-site POST status = %d, want 403 before any handler runs", target, rec.Code)
		}
	}
}
