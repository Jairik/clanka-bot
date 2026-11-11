package api_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

func newTestServer(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return api.NewHandler(p, logger, true), &logs
}

func postTranscript(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/debug/transcript", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
}

func TestDebugTranscriptReturnsSpokenTime(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := postTranscript(t, handler, `{"text":"what time is it"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reply     string `json:"reply"`
		Tool      string `json:"tool"`
		Path      string `json:"path"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if want := "It's 3:04 PM."; body.Reply != want {
		t.Errorf("reply = %q, want %q", body.Reply, want)
	}
	if body.Tool != clock.Name {
		t.Errorf("tool = %q, want %q", body.Tool, clock.Name)
	}
	if body.Path != string(tool.PathControlPhrase) {
		t.Errorf("path = %q", body.Path)
	}
	headerID := rec.Header().Get("X-Request-ID")
	if headerID == "" {
		t.Fatal("response has no X-Request-ID header")
	}
	if body.RequestID != headerID {
		t.Errorf("body request_id %q != header %q", body.RequestID, headerID)
	}
}

func TestDebugTranscriptRequiresText(t *testing.T) {
	handler, logs := newTestServer(t)
	rec := postTranscript(t, handler, `{"text":"   "}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "text") {
		t.Errorf("error body = %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "request_id") {
		t.Errorf("rejected request was not logged: %s", logs.String())
	}
	if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %s", len(lines), logs.String())
	}
}

func TestDebugTranscriptRejectsUnknownFields(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := postTranscript(t, handler, `{"text":"hi","extra":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDebugEntryDisabledReturns404(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	handler := api.NewHandler(p, logger, false)
	rec := postTranscript(t, handler, `{"text":"what time is it"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDebugEntryOnlyAcceptsPost(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/transcript", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHealthzRejectsPost(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
