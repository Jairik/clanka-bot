package spotify_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

func TestClientAttachesTheBearerTokenAndRetriesOne401(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	var calls atomic.Int32
	f.fake.mu.Lock()
	f.fake.api = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer access-1":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"status":401,"message":"expired"}}`))
		case "Bearer access-2":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_playing":false}`))
		default:
			t.Errorf("unexpected Authorization header")
			w.WriteHeader(http.StatusForbidden)
		}
	}
	f.fake.mu.Unlock()
	client := f.auth.Client()
	if client == nil {
		t.Fatal("Client() is nil")
	}
	body, err := client.Get(f.ctx, "/me/player", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(string(body), "is_playing") {
		t.Fatalf("body = %s", body)
	}
	if calls.Load() != 2 {
		t.Fatalf("API calls = %d, want one 401 and one retry", calls.Load())
	}
}

func TestClientRejectsAnAbsolutePath(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	client := f.auth.Client()
	if _, err := client.Get(f.ctx, "https://evil.example/v1/me", nil); err == nil {
		t.Fatal("an absolute URL was accepted")
	}
	if _, err := client.Get(f.ctx, "me/player", nil); err == nil {
		t.Fatal("a path without a leading slash was accepted")
	}
}

func TestClientSurfacesOnlyTheSafeAPIReason(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	f.fake.mu.Lock()
	f.fake.api = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"reason":"NO_ACTIVE_DEVICE","message":"private detail"}}`))
	}
	f.fake.mu.Unlock()
	client := f.auth.Client()
	_, err := client.Get(f.ctx, "/me/player", nil)
	var apiErr *spotify.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Get error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Reason != "NO_ACTIVE_DEVICE" {
		t.Fatalf("APIError = %+v", apiErr)
	}
	if strings.Contains(err.Error(), "private detail") {
		t.Fatal("the API message was surfaced")
	}
}

func TestClientPutAccepts204(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	f.fake.mu.Lock()
	f.fake.api = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}
	f.fake.mu.Unlock()
	client := f.auth.Client()
	body, err := client.Put(f.ctx, "/me/player/pause", url.Values{"device_id": {"abc"}}, nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("204 body = %q", body)
	}
}

func TestClientDoesNotFollowRedirectsToAnotherOrigin(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	f.fake.mu.Lock()
	f.fake.api = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}
	f.fake.mu.Unlock()
	_, err := f.auth.Client().Get(f.ctx, "/me/player", nil)
	var apiErr *spotify.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("Get error = %v, want a bounded 307 APIError", err)
	}
	if targetHits.Load() != 0 {
		t.Fatal("the redirect target received the bearer token")
	}
}
