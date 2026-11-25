package dashboard_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/hermes"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// TestHermesRuntimeAPI exercises the owner and Origin guards on the real router.
func TestHermesRuntimeAPI(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := settings.New(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	var cloudKey string
	// Exercise local readiness without giving the Assistant the local key.
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"status":"ok","local_ready":true,"local_base_url":"http://private-endpoint/v1","local_model":"ornith:9b"}`)
	}))
	t.Cleanup(sidecar.Close)
	runtime, err := hermes.New(ctx, st.DB(), sidecar.URL+"/run", strings.Repeat("t", 32), hermes.RuntimeConfig{
		Settings: svc, LocalBaseURL: "http://private-endpoint/v1", LocalModel: "ornith:9b",
		OpenCodeKey: func() string { return cloudKey },
	}, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	handler := newHandler(t, dashboard.Options{HermesRuntime: runtime})
	const path = "/api/ui/hermes/runtime"
	const origin = "http://127.0.0.1:8082"
	const local = `{"provider":"local","open_code_go_model":"deepseek-v4-pro"}`
	const cloud = `{"provider":"opencode-go","open_code_go_model":"kimi-k3"}`
	for _, tc := range []struct {
		name, method, login, origin, body string
		status                            int
	}{
		{"owner reads", http.MethodGet, owner, "", "", http.StatusOK},
		{"wrong owner reads", http.MethodGet, "other@example.net", "", "", http.StatusForbidden},
		{"wrong owner writes", http.MethodPost, "other@example.net", origin, local, http.StatusForbidden},
		{"cross origin", http.MethodPost, owner, "https://other.example", local, http.StatusForbidden},
		{"missing origin", http.MethodPost, owner, "", local, http.StatusForbidden},
		{"endpoint field", http.MethodPost, owner, origin, `{"provider":"local","open_code_go_model":"kimi-k3","url":"https://other.example"}`, http.StatusBadRequest},
		{"unknown provider", http.MethodPost, owner, origin, strings.Replace(local, "local", "auto", 1), http.StatusBadRequest},
		{"unknown model", http.MethodPost, owner, origin, strings.Replace(local, "deepseek-v4-pro", "anything", 1), http.StatusBadRequest},
		{"trailing JSON", http.MethodPost, owner, origin, local + "{}", http.StatusBadRequest},
		{"cloud key missing", http.MethodPost, owner, origin, cloud, http.StatusBadRequest},
		{"save cloud preference locally", http.MethodPost, owner, origin, local, http.StatusOK},
		{"select cloud", http.MethodPost, owner, origin, cloud, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "select cloud" {
				cloudKey = "private-cloud-key"
			}
			before := runtime.Runtime().Selection
			req := request(tc.method, path, tc.login)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", "application/json")
			req.Body = io.NopCloser(strings.NewReader(tc.body))
			rec := serve(handler, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status != http.StatusOK {
				if runtime.Runtime().Selection != before {
					t.Fatal("rejected request changed the selection")
				}
				if tc.status == http.StatusBadRequest {
					var problem map[string]string
					decodeJSON(t, rec, &problem)
					if len(problem) != 1 || problem["error"] == "" || (tc.body == cloud && !strings.Contains(problem["error"], "key")) {
						t.Fatalf("error response = %v", problem)
					}
				}
				return
			}
			if strings.Contains(rec.Body.String(), "private-") {
				t.Fatalf("response exposed a connection detail: %s", rec.Body)
			}
			var view hermes.RuntimeView
			decodeJSON(t, rec, &view)
			if !view.LocalReady || view.LocalModel != "ornith:9b" || len(view.OpenCodeModels) != 4 || view.OpenCodeReady != (cloudKey != "") {
				t.Fatalf("runtime response = %+v", view)
			}
			switch tc.body {
			case local:
				if view.Provider != "local" || view.OpenCodeModel != "deepseek-v4-pro" {
					t.Fatalf("local preference = %+v", view.Selection)
				}
			case cloud:
				if view.Provider != "opencode-go" || view.OpenCodeModel != "kimi-k3" {
					t.Fatalf("cloud selection = %+v", view.Selection)
				}
			}
		})
	}
}

// TestHermesRuntimeFirstRun exercises the same unwired dashboard used by dashsmoke.
func TestHermesRuntimeFirstRun(t *testing.T) {
	handler := newHandler(t, dashboard.Options{})
	const path = "/api/ui/hermes/runtime"
	var view hermes.RuntimeView
	if rec := getJSON(t, handler, path, &view); rec.Code != http.StatusOK ||
		view.Provider != "opencode-go" || view.OpenCodeModel != "deepseek-v4.1-flash" ||
		view.LocalReady || view.OpenCodeReady || len(view.OpenCodeModels) != 4 {
		t.Fatalf("first run response = %d %s", rec.Code, rec.Body)
	}
	req := request(http.MethodPost, path, owner)
	req.Header.Set("Origin", "http://127.0.0.1:8082")
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(strings.NewReader(`{"provider":"local","open_code_go_model":"kimi-k3"}`))
	if rec := serve(handler, req); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first run mutation = %d %s", rec.Code, rec.Body)
	}
}
