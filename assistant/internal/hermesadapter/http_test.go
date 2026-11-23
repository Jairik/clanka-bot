package hermesadapter_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
)

const testToken = "hermes-runtime-token-for-adapter-tests"

func TestHTTPRequiresBearerAndOmitsSchemasOnList(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	handler := mustHandler(t, f)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tools", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d", rec.Code)
	}

	rec = doHTTP(t, handler, http.MethodGet, "/tools", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"weather.current"`) || !strings.Contains(body, `"weather.forecast"`) {
		t.Fatalf("list missing weather Tools: %s", body)
	}
	if strings.Contains(body, "input_schema") || strings.Contains(body, "output_schema") {
		t.Fatalf("list included schemas: %s", body)
	}

	rec = doHTTP(t, handler, http.MethodGet, "/tools/weather.current", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("schema status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "input_schema") {
		t.Fatalf("schema omitted input_schema: %s", rec.Body.String())
	}

	rec = doHTTP(t, handler, http.MethodGet, "/tools/github.pr_list", "", testToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden schema status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPInvokeRunsAllowlistedTool(t *testing.T) {
	f := newFixture(t)
	f.grantWeather(t)
	handler := mustHandler(t, f)

	rec := doHTTP(t, handler, http.MethodPost, "/tools/weather.current", `{"arguments":{"city":"Lisbon"}}`, testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invoke status = %d body = %s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Name   string          `json:"name"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.Name != "weather.current" || !strings.Contains(string(reply.Result), "Lisbon") {
		t.Fatalf("reply = %s", rec.Body.String())
	}

	rec = doHTTP(t, handler, http.MethodPost, "/tools/github.issue_create", `{"arguments":{}}`, testToken)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowed") {
		t.Fatalf("forbidden invoke = %d %s", rec.Code, rec.Body.String())
	}
	if f.calls["github.issue_create"] != 0 {
		t.Fatal("confirm handler ran through HTTP")
	}
}

func TestNewHandlerRefusesAShortToken(t *testing.T) {
	f := newFixture(t)
	if got := hermesadapter.NewHandler(f.adapter, "short"); got != nil {
		t.Fatal("short token installed a handler")
	}
}

func mustHandler(t *testing.T, f *fixture) http.Handler {
	t.Helper()
	handler := hermesadapter.NewHandler(f.adapter, testToken)
	if handler == nil {
		t.Fatal("handler is nil")
	}
	return handler
}

func doHTTP(t *testing.T, handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
