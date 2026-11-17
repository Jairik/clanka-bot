package dashboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type fakeExplainer struct {
	decision pipeline.RoutingDecision
}

// Explain implements dashboard.RoutingExplainer for tests.
func (f *fakeExplainer) Explain(ctx context.Context, transcript string) pipeline.RoutingDecision {
	return f.decision
}

// makeExplainRequest builds a test request with Tailscale identity and CSRF headers.
func makeExplainRequest(method, target string, values url.Values, user, site string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	r.Host = "127.0.0.1:8082"
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Tailscale-User-Login", user)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", site)
	return r
}

// TestRoutingExplainEndpointSecurity verifies that POST /api/ui/routing/explain
// rejects non-owner and cross-origin requests like every other mutating route.
func TestRoutingExplainEndpointSecurity(t *testing.T) {
	explainer := &fakeExplainer{
		decision: pipeline.RoutingDecision{
			Path:     tool.PathFast,
			Tool:     "lights.off",
			Decision: "fast path: lights.off",
		},
	}
	h, err := dashboard.New(dashboard.Options{
		Owner:     "owner@example.com",
		Explainer: explainer,
	})
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}

	body := url.Values{"text": {"turn off the lights"}}

	// Valid request from owner with same-origin header: 200 OK.
	rec := httptest.NewRecorder()
	req := makeExplainRequest("POST", "/api/ui/routing/explain", body, "owner@example.com", "same-origin")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Non-owner request: 403 Forbidden.
	rec = httptest.NewRecorder()
	req = makeExplainRequest("POST", "/api/ui/routing/explain", body, "intruder@example.com", "same-origin")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", rec.Code)
	}

	// Cross-origin request: 403 Forbidden.
	rec = httptest.NewRecorder()
	req = makeExplainRequest("POST", "/api/ui/routing/explain", body, "owner@example.com", "cross-site")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", rec.Code)
	}
}
