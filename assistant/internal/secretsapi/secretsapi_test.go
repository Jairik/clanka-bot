package secretsapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/secretsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

const apiSecretName = "SECRETS_API_TEST_KEY"

// newHandler builds the real secrets service over a real SQLite file and wraps
// it in the composable HTTP handler.
func newHandler(t *testing.T, logger *slog.Logger) (http.Handler, *secrets.Service) {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	svc, err := secrets.New(context.Background(), st.DB(), []string{apiSecretName})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	return secretsapi.New(svc, logger), svc
}

// call performs one request and returns the recorder.
func call(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestSetReturnsMetadataOnlyAndListNeverCarriesTheValue(t *testing.T) {
	t.Setenv(apiSecretName, "")
	handler, _ := newHandler(t, nil)

	secret := "sk-or-v1-abcdefghijkl"
	rec := call(t, handler, http.MethodPut, "/secrets/"+apiSecretName, `{"value":"`+secret+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("PUT response contains the secret value: %s", rec.Body.String())
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if meta["name"] != apiSecretName || meta["set"] != true || meta["source"] != "store" {
		t.Errorf("PUT metadata = %v", meta)
	}
	if meta["last4"] != "ijkl" {
		t.Errorf("last4 = %v, want ijkl", meta["last4"])
	}
	if meta["updated_at"] == nil {
		t.Error("updated_at is missing after a stored write")
	}

	for _, path := range []string{"/secrets", "/secrets/" + apiSecretName} {
		rec := call(t, handler, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("GET %s contains the secret value: %s", path, rec.Body.String())
		}
	}
}

func TestClearFallsBackToTheEnvironmentBootstrap(t *testing.T) {
	const envValue = "env-value-123456"
	t.Setenv(apiSecretName, envValue)
	handler, _ := newHandler(t, nil)

	rec := call(t, handler, http.MethodDelete, "/secrets/"+apiSecretName, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode DELETE response: %v", err)
	}
	if meta["source"] != "env" || meta["set"] != true {
		t.Errorf("DELETE metadata = %v, want the environment source", meta)
	}
	if _, present := meta["updated_at"]; present {
		t.Errorf("DELETE metadata has updated_at for the environment layer: %v", meta)
	}
	if strings.Contains(rec.Body.String(), envValue) {
		t.Fatalf("DELETE response contains the environment value: %s", rec.Body.String())
	}
}

func TestShortSecretsDoNotLeakThroughLastFour(t *testing.T) {
	t.Setenv(apiSecretName, "")
	handler, _ := newHandler(t, nil)

	rec := call(t, handler, http.MethodPut, "/secrets/"+apiSecretName, `{"value":"abcd"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if meta["last4"] != "" {
		t.Errorf("last4 = %v for a four-character secret, want empty", meta["last4"])
	}
	if strings.Contains(rec.Body.String(), "abcd") {
		t.Fatalf("response exposes the short secret: %s", rec.Body.String())
	}
}

func TestRequestErrorsAreSafeAndDoNotEchoTheValue(t *testing.T) {
	t.Setenv(apiSecretName, "")
	handler, _ := newHandler(t, nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{name: "unknown secret", method: http.MethodPut, path: "/secrets/UNDECLARED", body: `{"value":"value-12345678"}`, status: http.StatusNotFound},
		{name: "empty value", method: http.MethodPut, path: "/secrets/" + apiSecretName, body: `{"value":"   "}`, status: http.StatusBadRequest},
		{name: "unknown field", method: http.MethodPut, path: "/secrets/" + apiSecretName, body: `{"value":"value-12345678","extra":true}`, status: http.StatusBadRequest},
		{name: "invalid json", method: http.MethodPut, path: "/secrets/" + apiSecretName, body: `{"value":`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(t, handler, tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "value-12345678") {
				t.Fatalf("error response echoed the submitted value: %s", rec.Body.String())
			}
		})
	}
}

func TestLogsNeverContainTheSecretValue(t *testing.T) {
	t.Setenv(apiSecretName, "")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler, _ := newHandler(t, logger)

	secret := "log-secret-abcdefghijkl"
	rec := call(t, handler, http.MethodPut, "/secrets/"+apiSecretName, `{"value":"`+secret+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("logs contain the secret value: %s", logs.String())
	}
}
