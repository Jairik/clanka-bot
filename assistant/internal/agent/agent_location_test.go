package agent_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// fakeGeolocation serves the documented ipwho.is shape, coordinates included,
// and changes city after the first lookup.
func fakeGeolocation(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"success":true,"city":"London","region":"England",
				"latitude":51.5074,"longitude":-0.1278,"timezone":{"id":"Europe/London"}}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"city":"Paris","region":"Ile-de-France",
			"latitude":48.8566,"longitude":2.3522,"timezone":{"id":"Europe/Paris"}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func newLocationService(t *testing.T, server *httptest.Server) *location.Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return location.NewService(
		location.NewIPWhoIs(server.Client(), server.URL),
		location.Location{},
		24*time.Hour,
		logger,
	)
}

// TestAgentRequestCarriesPlaceButNeverCoordinates is acceptance criterion 2:
// latitude and longitude cannot appear in the outgoing agent request.
func TestAgentRequestCarriesPlaceButNeverCoordinates(t *testing.T) {
	service := newLocationService(t, fakeGeolocation(t))
	service.Refresh(context.Background())
	if _, ok := service.Current(); !ok {
		t.Fatal("location lookup produced no place")
	}

	fake := newFakeProvider(t, textReply("Hello."))
	settings := testSettings(fake.start().URL)
	settings.SystemPromptFunc = func() string { return service.SystemPrompt("Be brief.") }
	a := newTestAgent(t, settings, nil, nil)

	if got := a.Run(context.Background(), tool.Request{ID: "req-loc"}, "hello", nil); got.Text != "Hello." {
		t.Fatalf("reply = %q", got.Text)
	}

	raw := bodyString(t, fake.request(t, 0).Body)
	for _, want := range []string{"Be brief.", "London", "England", "Europe/London"} {
		if !strings.Contains(raw, want) {
			t.Errorf("request is missing %q: %s", want, raw)
		}
	}
	for _, banned := range []string{"latitude", "longitude", "51.5074", "-0.1278"} {
		if strings.Contains(raw, banned) {
			t.Errorf("request leaked %q: %s", banned, raw)
		}
	}
}

func TestAgentSystemPromptFollowsLocationRefresh(t *testing.T) {
	service := newLocationService(t, fakeGeolocation(t))
	fake := newFakeProvider(t, textReply("One."), textReply("Two."))
	settings := testSettings(fake.start().URL)
	settings.SystemPromptFunc = func() string { return service.SystemPrompt("") }
	a := newTestAgent(t, settings, nil, nil)

	service.Refresh(context.Background())
	a.Run(context.Background(), tool.Request{ID: "req-1"}, "where am I", nil)
	service.Refresh(context.Background())
	a.Run(context.Background(), tool.Request{ID: "req-2"}, "and now", nil)

	first := bodyString(t, fake.request(t, 0).Body)
	second := bodyString(t, fake.request(t, 1).Body)
	if !strings.Contains(first, "London") || strings.Contains(first, "Paris") {
		t.Errorf("first request = %s, want London", first)
	}
	if !strings.Contains(second, "Paris") || strings.Contains(second, "London") {
		t.Errorf("second request = %s, want Paris after refresh", second)
	}
}

func TestAgentFallsBackToDefaultPromptWithoutPlace(t *testing.T) {
	service := newLocationService(t, fakeGeolocation(t))
	// No refresh: no last value and no config fallback.
	fake := newFakeProvider(t, textReply("Hello."))
	settings := testSettings(fake.start().URL)
	settings.SystemPromptFunc = func() string { return service.SystemPrompt("") }
	a := newTestAgent(t, settings, nil, nil)
	a.Run(context.Background(), tool.Request{ID: "req-loc"}, "hello", nil)

	raw := bodyString(t, fake.request(t, 0).Body)
	if !strings.Contains(raw, agent.DefaultSystemPrompt) {
		t.Errorf("request is missing the default system prompt: %s", raw)
	}
}
