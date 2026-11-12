package settingsapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/settingsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
)

type in struct {
	Text string `json:"text"`
}

type out struct {
	Text string `json:"text"`
}

// apiFixture wires a real registry, settings service and policy behind the
// public HTTP handler. No internal collaborator is mocked.
type apiFixture struct {
	server   *httptest.Server
	registry *tool.Registry
	policy   *toolpolicy.Policy
	store    *store.Store
}

func newFixture(t *testing.T) *apiFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	reg := tool.NewRegistry(2 * time.Second)
	for _, def := range []struct {
		name       string
		permission tool.Permission
	}{
		{"test.read", tool.PermissionRead},
		{"test.act", tool.PermissionAct},
	} {
		def := def
		if err := tool.Add(reg, tool.Tool[in, out]{
			Name:        def.name,
			Description: "Test tool.",
			Permission:  def.permission,
			LocalOnly:   tool.LocalOnlyNo,
			Handler: func(_ context.Context, args in) (out, error) {
				return out{Text: args.Text}, nil
			},
			Speak: func(result out) string { return result.Text },
		}); err != nil {
			t.Fatalf("Add %s: %v", def.name, err)
		}
	}
	defs, err := toolpolicy.Definitions(reg, toolpolicy.Config{})
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	defs = append(defs, settings.String("agent.filler", "One moment.", settings.WithConfig("Just a second.")))
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	policy, err := toolpolicy.New(svc, reg)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}
	handler := settingsapi.New(svc, policy, nil)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &apiFixture{server: server, registry: reg, policy: policy, store: st}
}

func (f *apiFixture) request(t *testing.T, method, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var decoded map[string]any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp, decoded
}

func (f *apiFixture) execute(t *testing.T, name string) error {
	t.Helper()
	_, err := f.registry.Execute(context.Background(), name, json.RawMessage(`{"text":"hello"}`))
	return err
}

func settingByName(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	list, ok := body["settings"].([]any)
	if !ok {
		t.Fatalf("settings response = %#v, want a settings list", body)
	}
	for _, item := range list {
		setting, ok := item.(map[string]any)
		if ok && setting["key"] == key {
			return setting
		}
	}
	t.Fatalf("setting %q not found in %#v", key, body)
	return nil
}

func TestSettingsEndpointReportsEffectiveValueAndLayer(t *testing.T) {
	f := newFixture(t)

	resp, body := f.request(t, http.MethodGet, "/settings/agent.filler", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	if body["value"] != "Just a second." || body["source"] != string(settings.SourceConfig) {
		t.Fatalf("agent.filler = %#v, want the config layer", body)
	}
	if body["default"] != "One moment." || body["config"] != "Just a second." || body["override"] != nil {
		t.Fatalf("agent.filler layers = %#v", body)
	}

	resp, body = f.request(t, http.MethodPut, "/settings/agent.filler", map[string]any{"value": "Hold on."})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	if body["value"] != "Hold on." || body["source"] != string(settings.SourceOverride) {
		t.Fatalf("after PUT = %#v", body)
	}

	resp, body = f.request(t, http.MethodDelete, "/settings/agent.filler", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", resp.StatusCode)
	}
	if body["value"] != "Just a second." || body["source"] != string(settings.SourceConfig) {
		t.Fatalf("after DELETE = %#v, want the config layer", body)
	}
}

func TestSettingsEndpointListsEverySetting(t *testing.T) {
	f := newFixture(t)
	resp, body := f.request(t, http.MethodGet, "/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	for _, key := range []string{"agent.filler", "tools.test.read.enabled", "tools.test.read.permission"} {
		settingByName(t, body, key)
	}
}

func TestToolEndpointDisablesAndReEnablesTheNextRequest(t *testing.T) {
	f := newFixture(t)
	if err := f.execute(t, "test.read"); err != nil {
		t.Fatalf("initial Execute: %v", err)
	}

	resp, body := f.request(t, http.MethodPut, "/tools/test.read", map[string]any{"enabled": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	if body["enabled"] != false || body["enabled_source"] != string(settings.SourceOverride) {
		t.Fatalf("after disable = %#v", body)
	}
	if err := f.execute(t, "test.read"); !errors.Is(err, tool.ErrToolDisabled) {
		t.Fatalf("Execute while disabled = %v, want ErrToolDisabled", err)
	}

	resp, body = f.request(t, http.MethodPut, "/tools/test.read", map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusOK || body["enabled"] != true {
		t.Fatalf("re-enable = %d %#v", resp.StatusCode, body)
	}
	if err := f.execute(t, "test.read"); err != nil {
		t.Fatalf("Execute after re-enable: %v", err)
	}

	resp, body = f.request(t, http.MethodDelete, "/tools/test.read", nil)
	if resp.StatusCode != http.StatusOK || body["enabled"] != true || body["enabled_source"] != string(settings.SourceDefault) {
		t.Fatalf("reset = %d %#v", resp.StatusCode, body)
	}
}

func TestToolEndpointRaisesPermission(t *testing.T) {
	f := newFixture(t)
	resp, body := f.request(t, http.MethodPut, "/tools/test.read", map[string]any{"permission": "act"})
	if resp.StatusCode != http.StatusOK || body["permission"] != "act" || body["declared_permission"] != "read" {
		t.Fatalf("read -> act = %d %#v", resp.StatusCode, body)
	}
	if err := f.execute(t, "test.read"); err != nil {
		t.Fatalf("Execute at act: %v", err)
	}

	resp, body = f.request(t, http.MethodPut, "/tools/test.read", map[string]any{"permission": "confirm"})
	if resp.StatusCode != http.StatusOK || body["permission"] != "confirm" {
		t.Fatalf("act -> confirm = %d %#v", resp.StatusCode, body)
	}
	if err := f.execute(t, "test.read"); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("Execute at confirm = %v, want ErrConfirmationRequired", err)
	}
}

func TestToolEndpointRejectsLooseningLocalOnlyAndBadRequests(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"loosen act to read", http.MethodPut, "/tools/test.act", map[string]any{"permission": "read"}, http.StatusBadRequest},
		{"change local-only", http.MethodPut, "/tools/test.read", map[string]any{"local_only": true}, http.StatusBadRequest},
		{"empty update", http.MethodPut, "/tools/test.read", map[string]any{}, http.StatusBadRequest},
		{"unknown field", http.MethodPut, "/tools/test.read", map[string]any{"bogus": true}, http.StatusBadRequest},
		{"unknown tool", http.MethodPut, "/tools/test.missing", map[string]any{"enabled": false}, http.StatusNotFound},
		{"unknown tool read", http.MethodGet, "/tools/test.missing", nil, http.StatusNotFound},
		{"unknown setting", http.MethodGet, "/settings/agent.missing", nil, http.StatusNotFound},
		{"wrong type", http.MethodPut, "/settings/agent.filler", map[string]any{"value": 42}, http.StatusBadRequest},
		{"missing value", http.MethodPut, "/settings/agent.filler", map[string]any{}, http.StatusBadRequest},
		{"loosen via settings", http.MethodPut, "/settings/tools.test.act.permission", map[string]any{"value": "read"}, http.StatusBadRequest},
		{"local-only via settings", http.MethodPut, "/settings/tools.test.read.local_only", map[string]any{"value": true}, http.StatusNotFound},
		{"wrong method", http.MethodPost, "/settings", nil, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := f.request(t, tt.method, tt.path, tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d (body %#v)", resp.StatusCode, tt.want, body)
			}
			if tt.want != http.StatusMethodNotAllowed {
				if _, ok := body["error"]; !ok {
					t.Errorf("body = %#v, want an error message", body)
				}
			}
		})
	}
	// The rejected loosen left the live value untouched.
	if got := f.policy.State("test.act").Permission; got != tool.PermissionAct {
		t.Errorf("test.act permission = %q after rejected loosen, want act", got)
	}
}

func TestGenericSettingsHandlerWorksWithoutAToolPolicy(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	svc, err := settings.New(ctx, st, settings.String("timezone", "UTC"))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	server := httptest.NewServer(settingsapi.New(svc, nil, nil))
	defer server.Close()

	resp, err := http.Get(server.URL + "/settings/timezone")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	resp, err = http.Get(server.URL + "/tools")
	if err != nil {
		t.Fatalf("GET tools: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /tools status = %d, want 404 without a policy", resp.StatusCode)
	}
}
