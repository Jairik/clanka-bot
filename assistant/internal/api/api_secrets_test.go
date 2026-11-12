package api_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/secretsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/settingsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// TestSecretsStayOutOfTheSettingsAPI builds the same /api mux main mounts:
// setting a secret must never add it to the settings listing, and the secrets
// route must only return metadata.
func TestSecretsStayOutOfTheSettingsAPI(t *testing.T) {
	const secretName = "API_TEST_SECRET_KEY"
	const secretValue = "api-test-secret-abcdefghijkl"
	t.Setenv(secretName, "")

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	settingsSvc, err := settings.New(context.Background(), st, settings.String("greeting", "hello"))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	secretsSvc, err := secrets.New(context.Background(), st.DB(), []string{secretName})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}

	apiMux := http.NewServeMux()
	mount := func(prefix string, handler http.Handler) {
		apiMux.Handle(prefix, handler)
		apiMux.Handle(prefix+"/", handler)
	}
	settingsHandler := settingsapi.New(settingsSvc, nil, nil)
	mount("/settings", settingsHandler)
	mount("/tools", settingsHandler)
	mount("/secrets", secretsapi.New(secretsSvc, nil))

	handler := api.NewHandler(nil, nil, true,
		api.WithAPI(apiMux),
		api.WithDebugAllowlist(func(ip net.IP) bool { return ip.IsLoopback() }),
	)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:40000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(http.MethodPut, "/api/secrets/"+secretName, `{"value":"`+secretValue+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT secret status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	settingsBody := do(http.MethodGet, "/api/settings", "").Body.String()
	if strings.Contains(settingsBody, secretName) {
		t.Fatalf("settings listing names the secret: %s", settingsBody)
	}
	if strings.Contains(settingsBody, secretValue) {
		t.Fatalf("settings listing contains the secret value: %s", settingsBody)
	}

	secretsBody := do(http.MethodGet, "/api/secrets", "").Body.String()
	if !strings.Contains(secretsBody, secretName) {
		t.Fatalf("secrets listing is missing %s: %s", secretName, secretsBody)
	}
	if strings.Contains(secretsBody, secretValue) {
		t.Fatalf("secrets listing contains the value: %s", secretsBody)
	}
}
