package dashboard_test

import (
	"net/http"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
)

// TestDashboardRejectsAHostileHostEvenWithOwnerAndSameOrigin is the S2
// regression: a DNS-rebinding page can set the identity header and a
// same-origin marker, but its Host is not the configured tailnet name.
func TestDashboardRejectsAHostileHostEvenWithOwnerAndSameOrigin(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})

	get := request(http.MethodGet, "/health", owner)
	get.Host = "attacker.example:8082"
	get.Header.Set("Origin", "http://attacker.example:8082")
	get.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := serve(handler, get); rec.Code != http.StatusForbidden {
		t.Fatalf("hostile Host GET status = %d, want 403", rec.Code)
	}

	post := request(http.MethodPost, "/activity/ask", owner)
	post.Host = "attacker.example:8082"
	post.Header.Set("Origin", "http://attacker.example:8082")
	post.Header.Set("Sec-Fetch-Site", "same-origin")
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(handler, post); rec.Code != http.StatusForbidden {
		t.Fatalf("hostile Host POST status = %d, want 403", rec.Code)
	}
}

// TestDashboardRejectsAHostileHostOnEveryPage keeps the check ahead of every
// route, including static files, so a new page cannot silently opt out.
func TestDashboardRejectsAHostileHostOnEveryPage(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	paths := []string{
		"/", "/health", "/health/panel", "/activity", "/activity/recent",
		"/alerts", "/settings", "/secrets", "/sounds", "/voices", "/personas",
		"/spotify", "/static/app.css",
	}
	for _, path := range paths {
		req := request(http.MethodGet, path, owner)
		req.Host = "attacker.example"
		if rec := serve(handler, req); rec.Code != http.StatusForbidden {
			t.Errorf("%s with a hostile Host = %d, want 403", path, rec.Code)
		}
	}
}

// TestDashboardAcceptsTheConfiguredHostAndTheLoopbackSmoke keeps the normal
// tailscale serve host and the documented curl smoke working.
func TestDashboardAcceptsTheConfiguredHostAndTheLoopbackSmoke(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	for _, host := range []string{"pi.example.ts.net", "127.0.0.1:8082", "localhost:8082"} {
		req := request(http.MethodGet, "/health", owner)
		req.Host = host
		if rec := serve(handler, req); rec.Code != http.StatusOK {
			t.Errorf("Host %q status = %d, want 200", host, rec.Code)
		}
	}
}

// TestDashboardOAuthCallbackAcceptsTheConfiguredHost proves the cross-site
// Spotify callback is not blocked by the Host allowlist; its own state and
// binding checks decide the request instead.
func TestDashboardOAuthCallbackAcceptsTheConfiguredHost(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	req := request(http.MethodGet, "/spotify/callback", owner)
	req.Host = "pi.example.ts.net"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := serve(handler, req); rec.Code == http.StatusForbidden {
		t.Fatalf("the configured callback host was refused by the Host guard")
	}
}

func TestNewDashboardRejectsABadHostPattern(t *testing.T) {
	if _, err := dashboard.New(dashboard.Options{Owner: owner, Hosts: []string{"*.ts.net"}}); err == nil {
		t.Fatal("New accepted a wildcard host")
	}
}

// TestDashboardMountedAPIUsesTheSameHostGuard proves the /api/ mount cannot
// bypass the Host check: the handler only runs after identity, Host and origin
// all pass.
func TestDashboardMountedAPIUsesTheSameHostGuard(t *testing.T) {
	apiCalls := 0
	handler := newHandler(t, dashboard.Options{
		API: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			apiCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	})

	hostile := request(http.MethodGet, "/api/settings", owner)
	hostile.Host = "attacker.example"
	hostile.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := serve(handler, hostile); rec.Code != http.StatusForbidden {
		t.Fatalf("hostile Host API read = %d, want 403", rec.Code)
	}

	crossSite := request(http.MethodPost, "/api/settings/x", owner)
	crossSite.Header.Set("Content-Type", "application/json")
	crossSite.Header.Set("Origin", "https://evil.example")
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := serve(handler, crossSite); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site API write = %d, want 403", rec.Code)
	}

	ok := request(http.MethodGet, "/api/settings", owner)
	ok.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := serve(handler, ok); rec.Code != http.StatusOK {
		t.Fatalf("configured-host API read = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if apiCalls != 1 {
		t.Fatalf("API handler calls = %d, want 1", apiCalls)
	}
}
