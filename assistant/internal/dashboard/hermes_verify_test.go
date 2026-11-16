package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
)

type verifySecrets map[string]string

func (f verifySecrets) Get(name string) (string, bool) { v, ok := f[name]; return v, ok }

func verifyHandler(t *testing.T, secrets verifySecrets, base string) *Handler {
	t.Helper()
	v := integrations.NewVerifier(integrations.VerifierOptions{
		Secrets:    secrets,
		GitHubBase: base,
	})
	return &Handler{integrations: integrations.New(nil, nil), verifier: v}
}

func TestHermesVerifyReturnsResultInline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h := verifyHandler(t, verifySecrets{"GITHUB_TOKEN": "ghp_secret_value"}, srv.URL)

	req := httptest.NewRequest("POST", "/hermes/integrations/github/verify", nil)
	req.SetPathValue("id", "github")
	w := httptest.NewRecorder()
	h.hermesVerify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"class":"ok"`) {
		t.Fatalf("expected ok class, got %s", body)
	}
	// The token must never appear in the response.
	if strings.Contains(body, "ghp_secret_value") {
		t.Fatalf("token leaked into response: %s", body)
	}
}

func TestHermesVerifyMappedClasses(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusOK, "ok"},
		{http.StatusUnauthorized, "rejected"},
		{http.StatusForbidden, "rejected"},
		{http.StatusBadGateway, "unreachable"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		h := verifyHandler(t, verifySecrets{"GITHUB_TOKEN": "ghp_x"}, srv.URL)
		req := httptest.NewRequest("POST", "/hermes/integrations/github/verify", nil)
		req.SetPathValue("id", "github")
		w := httptest.NewRecorder()
		h.hermesVerify(w, req)
		srv.Close()
		if w.Code != http.StatusOK {
			t.Fatalf("%d: code %d", tc.status, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"class":"`+tc.want+`"`) {
			t.Fatalf("%d: want class %s, got %s", tc.status, tc.want, w.Body.String())
		}
	}
}

func TestHermesVerifyUnknownAccount(t *testing.T) {
	h := verifyHandler(t, verifySecrets{"GITHUB_TOKEN": "x"}, "http://127.0.0.1:1")
	req := httptest.NewRequest("POST", "/hermes/integrations/nope/verify", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	h.hermesVerify(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d", w.Code)
	}
}
