package dashboard_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

type listeningResp struct {
	Available bool
	Listening bool
	Until     string
	Error     string
}

func postListening(handler http.Handler, values url.Values, edit func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/ui/listening", strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if edit != nil {
		edit(req)
	}
	return serve(handler, req)
}

func openSettingsForListening(t *testing.T) (*settings.Service, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	svc, err := settings.New(context.Background(), st, voice.ListeningDefinitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc, st
}

// TestListeningRouteWritesBothValues verifies that GET returns the effective state,
// and POST writes both wakeword.listening and wakeword.listening_until in one request.
func TestListeningRouteWritesBothValues(t *testing.T) {
	settingsSvc, _ := openSettingsForListening(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	handler := newHandler(t, dashboard.Options{
		Settings: settingsSvc,
		Wakeword: dashboard.WakewordInfo{Addr: "wakeword:10400"},
		Now:      func() time.Time { return now },
	})

	// GET returns default active listening state.
	var initial listeningResp
	rec := getJSON(t, handler, "/api/ui/listening", &initial)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ui/listening = %d: %s", rec.Code, rec.Body.String())
	}
	if !initial.Available || !initial.Listening || initial.Until != "" {
		t.Fatalf("initial view = %+v, want available listening with empty until", initial)
	}

	// 1. Timed pause: duration=15m sets listening=true and until=now+15m.
	rec = postListening(handler, url.Values{"duration": {"15m"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST duration=15m = %d: %s", rec.Code, rec.Body.String())
	}
	var timed listeningResp
	decodeJSON(t, rec, &timed)
	wantUntil := now.Add(15 * time.Minute).Format(time.RFC3339)
	if !timed.Available || timed.Listening || timed.Until != wantUntil {
		t.Fatalf("timed view = %+v, want listening=false and until=%s", timed, wantUntil)
	}
	// Verify both values in the settings service.
	if !settingsSvc.Bool(voice.ListeningKey) {
		t.Errorf("settings listening = false, want true for timed pause")
	}
	if got := settingsSvc.String(voice.ListeningUntilKey); got != wantUntil {
		t.Errorf("settings until = %q, want %q", got, wantUntil)
	}

	// 2. Indefinite pause: listening=false sets listening=false and clears until.
	rec = postListening(handler, url.Values{"listening": {"false"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST listening=false = %d: %s", rec.Code, rec.Body.String())
	}
	var indef listeningResp
	decodeJSON(t, rec, &indef)
	if !indef.Available || indef.Listening || indef.Until != "" {
		t.Fatalf("indefinite view = %+v, want listening=false and until empty", indef)
	}
	if settingsSvc.Bool(voice.ListeningKey) {
		t.Errorf("settings listening = true, want false for indefinite pause")
	}
	if got := settingsSvc.String(voice.ListeningUntilKey); got != "" {
		t.Errorf("settings until = %q, want empty", got)
	}

	// 3. Resume: listening=true sets listening=true and clears until.
	rec = postListening(handler, url.Values{"listening": {"true"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST listening=true = %d: %s", rec.Code, rec.Body.String())
	}
	var resumed listeningResp
	decodeJSON(t, rec, &resumed)
	if !resumed.Available || !resumed.Listening || resumed.Until != "" {
		t.Fatalf("resumed view = %+v, want listening=true and until empty", resumed)
	}
	if !settingsSvc.Bool(voice.ListeningKey) {
		t.Errorf("settings listening = false, want true after resume")
	}
	if got := settingsSvc.String(voice.ListeningUntilKey); got != "" {
		t.Errorf("settings until = %q, want empty after resume", got)
	}

	// 4. Timed pause using minutes=30.
	rec = postListening(handler, url.Values{"minutes": {"30"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST minutes=30 = %d: %s", rec.Code, rec.Body.String())
	}
	var minResp listeningResp
	decodeJSON(t, rec, &minResp)
	wantMinUntil := now.Add(30 * time.Minute).Format(time.RFC3339)
	if minResp.Listening || minResp.Until != wantMinUntil {
		t.Fatalf("minutes=30 view = %+v, want until=%s", minResp, wantMinUntil)
	}

	// 5. Explicit pair write: listening=true and listening_until=<future>.
	explicitUntil := "2026-11-17T14:00:00Z"
	rec = postListening(handler, url.Values{
		"listening":       {"true"},
		"listening_until": {explicitUntil},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST explicit pair = %d: %s", rec.Code, rec.Body.String())
	}
	var explicit listeningResp
	decodeJSON(t, rec, &explicit)
	if explicit.Listening || explicit.Until != explicitUntil {
		t.Fatalf("explicit pair view = %+v, want until=%s", explicit, explicitUntil)
	}
	if !settingsSvc.Bool(voice.ListeningKey) || settingsSvc.String(voice.ListeningUntilKey) != explicitUntil {
		t.Errorf("settings pair mismatch: listening=%v, until=%s",
			settingsSvc.Bool(voice.ListeningKey), settingsSvc.String(voice.ListeningUntilKey))
	}
}

// TestListeningRouteIdentityHostAndSameOrigin covers the security checks on POST /api/ui/listening.
func TestListeningRouteIdentityHostAndSameOrigin(t *testing.T) {
	settingsSvc, _ := openSettingsForListening(t)
	handler := newHandler(t, dashboard.Options{
		Settings: settingsSvc,
		Wakeword: dashboard.WakewordInfo{Addr: "wakeword:10400"},
		Hosts:    []string{"pi.example.ts.net"},
	})

	// Valid same-origin request is accepted.
	rec := postListening(handler, url.Values{"listening": {"true"}}, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("valid request status = %d, want 200", rec.Code)
	}

	// Sec-Fetch-Site: cross-site is rejected.
	rec = postListening(handler, url.Values{"listening": {"true"}}, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", rec.Code)
	}

	// Missing origin metadata is rejected.
	rec = postListening(handler, url.Values{"listening": {"true"}}, func(r *http.Request) {
		r.Header.Del("Sec-Fetch-Site")
		r.Header.Del("Origin")
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing origin metadata status = %d, want 403", rec.Code)
	}

	// Non-owner is rejected.
	rec = postListening(handler, url.Values{"listening": {"true"}}, func(r *http.Request) {
		r.Header.Set("Tailscale-User-Login", "intruder@example.com")
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", rec.Code)
	}

	// Untrusted Host is rejected.
	rec = postListening(handler, url.Values{"listening": {"true"}}, func(r *http.Request) {
		r.Host = "evil.example"
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("untrusted host status = %d, want 403", rec.Code)
	}
}

// TestGentypesDiffFreeBuild ensures running cmd/gentypes produces no diff against types.ts on disk.
func TestGentypesDiffFreeBuild(t *testing.T) {
	typesPath := filepath.Join("..", "..", "web", "src", "types.ts")
	before, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatalf("read types.ts before gentypes: %v", err)
	}

	cmd := exec.Command("go", "run", "./cmd/gentypes")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./cmd/gentypes: %v\n%s", err, string(out))
	}

	after, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatalf("read types.ts after gentypes: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("web/src/types.ts has uncommitted changes after gentypes; run `cd assistant && go run ./cmd/gentypes`")
	}
}
