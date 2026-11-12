package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// announceSpeaker records what the queue speaks at the debug entry seam.
type announceSpeaker struct {
	phrases chan string
}

func newAnnounceSpeaker() *announceSpeaker {
	return &announceSpeaker{phrases: make(chan string, 8)}
}

func (s *announceSpeaker) SayText(_ context.Context, text string) error {
	s.phrases <- text
	return nil
}

func newAnnounceQueue(t *testing.T, speaker announce.Speaker) *announce.Queue {
	t.Helper()
	queue := announce.New(speaker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(queue.Close)
	return queue
}

func postAnnounce(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/debug/announce", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestDebugAnnounceQueuesAnAnnouncement(t *testing.T) {
	speaker := newAnnounceSpeaker()
	queue := newAnnounceQueue(t, speaker)
	handler, _ := newTestServerWith(t, api.WithAnnouncements(queue))

	rec := postAnnounce(t, handler, `{"text":"Dinner is ready."}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Queued bool `json:"queued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !body.Queued {
		t.Fatal("queued = false, want true")
	}

	select {
	case phrase := <-speaker.phrases:
		if phrase != "Dinner is ready." {
			t.Fatalf("spoken = %q, want the queued announcement", phrase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the announcement was never spoken")
	}
}

func TestDebugAnnounceRequiresText(t *testing.T) {
	queue := newAnnounceQueue(t, newAnnounceSpeaker())
	handler, logs := newTestServerWith(t, api.WithAnnouncements(queue))

	rec := postAnnounce(t, handler, `{"text":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "text") {
		t.Errorf("error body = %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "request_id") {
		t.Errorf("rejected request was not logged: %s", logs.String())
	}
}

func TestDebugAnnounceWithoutSpeechIsUnavailable(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := postAnnounce(t, handler, `{"text":"Dinner is ready."}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestDebugAnnounceOnlyAcceptsPost(t *testing.T) {
	queue := newAnnounceQueue(t, newAnnounceSpeaker())
	handler, _ := newTestServerWith(t, api.WithAnnouncements(queue))

	req := httptest.NewRequest(http.MethodGet, "/debug/announce", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestDebugAnnounceRejectsSiblingContainers(t *testing.T) {
	gateway := net.ParseIP("172.18.0.1")
	queue := newAnnounceQueue(t, newAnnounceSpeaker())
	handler, _ := newTestServerWith(t, api.WithAnnouncements(queue), api.WithDebugAllowlist(func(ip net.IP) bool {
		return ip.IsLoopback() || ip.Equal(gateway)
	}))

	req := httptest.NewRequest(http.MethodPost, "/debug/announce", strings.NewReader(`{"text":"Dinner is ready."}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "172.18.0.5:40000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestDebugAnnounceDisabledReturns404(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	queue := newAnnounceQueue(t, newAnnounceSpeaker())
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	handler := api.NewHandler(p, logger, false, api.WithAnnouncements(queue))
	rec := postAnnounce(t, handler, `{"text":"Dinner is ready."}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
