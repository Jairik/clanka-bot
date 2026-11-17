package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubHolder is a no-op Focus holder used to take a claim in tests.
type stubHolder struct{}

func (stubHolder) Intents() []focus.Intent { return nil }
func (stubHolder) HandleIntent(context.Context, focus.Intent) (string, bool) {
	return "", false
}

// silentSpeaker lets text be enqueued (Enqueue requires a non-nil speaker) and
// plays it without observable side effects.
type silentSpeaker struct{}

func (silentSpeaker) SayText(context.Context, string) error { return nil }

// TestFocusHandlerReturnsStateWithoutText pins that the API returns the three
// Focus priorities and the queue count/hold/playing flags, and that no
// announcement text can leak into the payload (#217).
func TestFocusHandlerReturnsStateWithoutText(t *testing.T) {
	reg := focus.New()
	reg.Claim(focus.Reply, stubHolder{})
	queue := announce.New(silentSpeaker{}, testLogger())
	t.Cleanup(queue.Close)
	// A held queue keeps the text enqueued but never played, so the snapshot
	// reports it as waiting without exposing the words.
	queue.Hold()
	queue.Enqueue("a private reminder the Dashboard must never see")

	handler := newHandler(t, dashboard.Options{Focus: reg, Announcements: queue})
	rec := serve(handler, request(http.MethodGet, "/api/ui/focus", owner))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"ring", "reply", "music", "announcements"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("focus payload missing %q: %s", key, rec.Body.String())
		}
	}
	var announcePayload map[string]json.RawMessage
	if err := json.Unmarshal(payload["announcements"], &announcePayload); err != nil {
		t.Fatalf("decode announcements: %v", err)
	}
	for _, key := range []string{"waiting", "held", "playing"} {
		if _, ok := announcePayload[key]; !ok {
			t.Fatalf("announcements missing %q: %s", key, payload["announcements"])
		}
	}
	// No announcement text may appear anywhere in the payload.
	if strings.Contains(rec.Body.String(), "private reminder") {
		t.Fatalf("announcement text leaked into payload: %s", rec.Body.String())
	}
	for _, banned := range []string{"message", "text", "Message", "Text"} {
		if strings.Contains(rec.Body.String(), fmt.Sprintf("\"%s\"", banned)) {
			t.Fatalf("payload contains a %q field: %s", banned, rec.Body.String())
		}
	}
}

// TestFocusClaimPublishesDelta pins that taking a Focus claim sends an SSE
// delta carrying the updated snapshot (#217).
func TestFocusClaimPublishesDelta(t *testing.T) {
	reg := focus.New()
	handler := newHandler(t, dashboard.Options{Focus: reg})
	server := httptest.NewServer(handler)
	defer server.Close()

	res, reader := openStream(t, server)
	defer res.Body.Close()

	// Take a claim; the registry's OnClaimed hook must push a "focus" delta.
	reg.Claim(focus.Reply, stubHolder{})

	name, data := readEvent(t, reader, 5*time.Second)
	if name != "focus" {
		t.Fatalf("event = %q, want focus", name)
	}
	var payload struct {
		Ring  bool `json:"ring"`
		Reply bool `json:"reply"`
		Music bool `json:"music"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if !payload.Reply || payload.Ring || payload.Music {
		t.Fatalf("focus delta = %+v, want reply held only", payload)
	}
}

// TestAnnouncementQueuePublishesDelta pins that queuing an announcement sends
// an SSE delta with the updated count and no text (#217).
func TestAnnouncementQueuePublishesDelta(t *testing.T) {
	queue := announce.New(nil, testLogger(), announce.WithPlayback())
	t.Cleanup(queue.Close)
	handler := newHandler(t, dashboard.Options{Announcements: queue})
	server := httptest.NewServer(handler)
	defer server.Close()

	res, reader := openStream(t, server)
	defer res.Body.Close()

	// EnqueuePlay works without a speaker and fires OnChange immediately.
	queue.EnqueuePlay(func(context.Context) error { return nil })

	name, data := readEvent(t, reader, 5*time.Second)
	if name != "focus" {
		t.Fatalf("event = %q, want focus", name)
	}
	if strings.Contains(strings.ToLower(data), "private") {
		t.Fatalf("announcement text leaked into delta: %s", data)
	}
	var payload struct {
		Announcements struct {
			Waiting int  `json:"waiting"`
			Held    bool `json:"held"`
			Playing bool `json:"playing"`
		} `json:"announcements"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if payload.Announcements.Waiting != 1 {
		t.Fatalf("waiting = %d, want 1", payload.Announcements.Waiting)
	}
}
