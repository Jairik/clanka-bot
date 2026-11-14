// Equalizer writes share authentication, validation and durable Custom storage.
package dashboard_test

import (
	"context"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/equalizer"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestEqualizerSave(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "eq.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := settings.New(ctx, st, equalizer.Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	h, err := dashboard.New(dashboard.Options{Owner: "owner@example.com", Settings: svc})
	if err != nil {
		t.Fatal(err)
	}
	custom := equalizer.Profile{Preset: "Custom", Bands: [10]int{4, -2}}
	for _, p := range []equalizer.Profile{custom, {Preset: "Flat"}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, settingsRequest("POST", "/api/ui/equalizer", url.Values{"profile": {equalizer.Encode(p)}}, "owner@example.com"))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	restored, err := settings.New(ctx, st, equalizer.Definitions()...)
	if err != nil {
		t.Fatal(err)
	}
	if restored.String(equalizer.CustomKey) != equalizer.Encode(custom) {
		t.Fatal("Custom was lost")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, settingsRequest("POST", "/api/ui/equalizer", url.Values{"profile": {`{}`}}, "owner@example.com"))
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, settingsRequest("POST", "/api/ui/equalizer", nil, "intruder@example.com"))
	if rec.Code != 403 {
		t.Fatal(rec.Code)
	}
}

// eqRequest includes the same identity and CSRF metadata as the Dashboard.
func settingsRequest(method, target string, values url.Values, owner string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	r.Host = "127.0.0.1:8082"
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Tailscale-User-Login", owner)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}
