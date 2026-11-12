package dashboard_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

const (
	spotifyTestClientID = "0123456789abcdef0123456789abcdef"
	spotifyTestRedirect = "http://127.0.0.1:8082/spotify/callback"
	spotifyTestCode     = "dashboard-code"
)

// spotifyTokenServer is the real HTTP boundary for the Dashboard OAuth tests.
// It validates the PKCE verifier, so the callback's exchange path is not
// stubbed.
type spotifyTokenServer struct {
	t      *testing.T
	server *httptest.Server

	mu             sync.Mutex
	challenge      string
	exchangeCalls  int
	refreshCalls   int
	accessSeq      int
	currentRefresh string
}

func newSpotifyTokenServer(t *testing.T) *spotifyTokenServer {
	t.Helper()
	fake := &spotifyTokenServer{t: t, currentRefresh: "refresh-1"}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *spotifyTokenServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		f.exchangeCalls++
		challenge := f.challenge
		f.mu.Unlock()
		if r.Form.Get("code") != spotifyTestCode || r.Form.Get("redirect_uri") != spotifyTestRedirect {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"dash-access-1","token_type":"Bearer","expires_in":3600,"refresh_token":"dash-refresh-1"}`)
	case "refresh_token":
		f.mu.Lock()
		f.refreshCalls++
		f.accessSeq++
		seq := f.accessSeq + 1
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"dash-access-`+string(rune('0'+seq))+`","token_type":"Bearer","expires_in":3600}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unsupported_grant_type"}`)
	}
}

func (f *spotifyTokenServer) counts() (exchange, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchangeCalls, f.refreshCalls
}

type spotifyFixture struct {
	t           *testing.T
	ctx         context.Context
	handler     *dashboard.Handler
	secrets     *secrets.Service
	auth        *spotify.Auth
	fake        *spotifyTokenServer
	logs        *lockedBuffer
	pairingPath string
}

func newSpotifyFixture(t *testing.T) *spotifyFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "spotify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := secrets.New(ctx, st.DB(), []string{spotify.SecretName})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	fake := newSpotifyTokenServer(t)
	cfg := spotify.DefaultConfig()
	cfg.ClientID = spotifyTestClientID
	cfg.RedirectURI = spotifyTestRedirect
	cfg.AuthorizeURL = fake.server.URL + "/authorize"
	cfg.TokenURL = fake.server.URL + "/token"
	logs := &lockedBuffer{}
	auth := spotify.NewAuth(spotify.Options{
		Config: cfg,
		Store:  svc,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	pairingPath := filepath.Join(t.TempDir(), "librespot.status")
	handler := newHandler(t, dashboard.Options{
		Secrets:        svc,
		Spotify:        auth,
		SpotifyPairing: &spotify.PairingReader{Path: pairingPath, StaleAfter: 45 * time.Second},
		Logger:         slog.New(slog.NewTextHandler(logs, nil)),
	})
	return &spotifyFixture{
		t: t, ctx: ctx, handler: handler, secrets: svc, auth: auth, fake: fake, logs: logs, pairingPath: pairingPath,
	}
}

func (f *spotifyFixture) writePairing(t *testing.T, state string) {
	t.Helper()
	body := "version=1\nstate=" + state + "\nsession=connected\ndevice=Clanka\nupdated=" + time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(f.pairingPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write pairing: %v", err)
	}
}

func (f *spotifyFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	return serve(f.handler, request(http.MethodGet, target, owner))
}

func (f *spotifyFixture) post(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := request(http.MethodPost, target, owner)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return serve(f.handler, req)
}

// startConnect performs the same-origin POST and returns the state and binding
// cookie from the redirect. It also teaches the fake token server the PKCE
// challenge from the authorize URL.
func (f *spotifyFixture) startConnect(t *testing.T) (string, string) {
	t.Helper()
	rec := f.post("/spotify/connect")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect status = %d, body = %s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("connect cookies = %d, want one binding cookie", len(cookies))
	}
	f.fake.mu.Lock()
	f.fake.challenge = parsed.Query().Get("code_challenge")
	f.fake.mu.Unlock()
	return parsed.Query().Get("state"), cookies[0].Value
}

func (f *spotifyFixture) callback(state, binding, query string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := request(http.MethodGet, "/spotify/callback?"+query+"&state="+url.QueryEscape(state), owner)
	if binding != "" {
		req.AddCookie(&http.Cookie{Name: "clanka_spotify_oauth", Value: binding})
	}
	return serve(f.handler, req)
}

func TestSpotifyPageShowsSeparatePairingAndWebAPIAuth(t *testing.T) {
	f := newSpotifyFixture(t)
	f.writePairing(t, "paired")
	rec := f.get("/spotify")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Spotify Connect (librespot)",
		"Web API auth",
		"Paired as Clanka",
		"not configured",
		"Connect Web API",
		spotifyTestRedirect,
		"user-modify-playback-state",
		"playlist-read-private",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(body, `href="/spotify"`) || !strings.Contains(body, `aria-current="page"`) {
		t.Error("nav does not mark the Spotify page current")
	}
	if strings.Contains(body, "dash-access") || strings.Contains(body, "dash-refresh") {
		t.Error("page rendered a Spotify token")
	}
}

func TestSpotifyConnectStartsAPKCEFlowOnlyFromASameOriginPost(t *testing.T) {
	f := newSpotifyFixture(t)
	rec := f.post("/spotify/connect")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect status = %d", rec.Code)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	query := location.Query()
	if query.Get("response_type") != "code" || query.Get("client_id") != spotifyTestClientID {
		t.Fatalf("authorize query = %v", query)
	}
	if query.Get("redirect_uri") != spotifyTestRedirect || query.Get("scope") != spotify.ScopeString() {
		t.Fatalf("authorize query = %v", query)
	}
	if query.Get("code_challenge_method") != "S256" || len(query.Get("code_challenge")) < 43 {
		t.Fatalf("PKCE query = %v", query)
	}
	if len(query.Get("state")) < 43 {
		t.Fatalf("state = %q", query.Get("state"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "clanka_spotify_oauth" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/spotify" {
		t.Fatalf("binding cookie = %+v", cookie)
	}

	// A cross-origin POST is rejected before it can start a flow.
	req := request(http.MethodPost, "/spotify/connect", owner)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := serve(f.handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin connect = %d, want 403", rec.Code)
	}
	if rec := f.get("/spotify/connect"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET connect = %d, want 405", rec.Code)
	}
}

func TestSpotifyCallbackStoresTheRefreshTokenAndRejectsReplay(t *testing.T) {
	f := newSpotifyFixture(t)
	state, binding := f.startConnect(t)
	rec := f.callback(state, binding, "code="+spotifyTestCode)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-connected" {
		t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if stored, ok := f.secrets.Get(spotify.SecretName); !ok || stored != "dash-refresh-1" {
		t.Fatalf("stored token = %q, %v", stored, ok)
	}
	page := f.get("/spotify").Body.String()
	if !strings.Contains(page, "connected") {
		t.Error("page does not show the Web API as connected")
	}
	if strings.Contains(page, "dash-access-1") || strings.Contains(page, "dash-refresh-1") {
		t.Error("page rendered a token after the callback")
	}
	if strings.Contains(f.logs.String(), "dash-access-1") || strings.Contains(f.logs.String(), "dash-refresh-1") || strings.Contains(f.logs.String(), state) {
		t.Errorf("logs contain a credential or state:\n%s", f.logs.String())
	}

	// Replaying the same callback is rejected and cannot exchange again.
	rec = f.callback(state, binding, "code="+spotifyTestCode)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-invalid-state" {
		t.Fatalf("replay = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if exchange, _ := f.fake.counts(); exchange != 1 {
		t.Fatalf("exchange calls = %d, want one", exchange)
	}
}

func TestSpotifyCallbackRejectsAWrongBindingCookie(t *testing.T) {
	f := newSpotifyFixture(t)
	state, _ := f.startConnect(t)
	rec := f.callback(state, "wrong-binding", "code="+spotifyTestCode)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-invalid-state" {
		t.Fatalf("wrong cookie = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := f.secrets.Get(spotify.SecretName); ok {
		t.Fatal("a wrong binding cookie stored a token")
	}
	if exchange, _ := f.fake.counts(); exchange != 0 {
		t.Fatal("a wrong binding cookie reached the token endpoint")
	}
}

func TestSpotifyCallbackHandlesDenialAndAForgedCode(t *testing.T) {
	f := newSpotifyFixture(t)
	state, binding := f.startConnect(t)
	rec := f.callback(state, binding, "error=access_denied")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-denied" {
		t.Fatalf("denial = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := f.secrets.Get(spotify.SecretName); ok {
		t.Fatal("a denied callback stored a token")
	}

	state, binding = f.startConnect(t)
	rec = f.callback(state, binding, "code=forged-code")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-exchange-failed" {
		t.Fatalf("forged code = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := f.secrets.Get(spotify.SecretName); ok {
		t.Fatal("a forged code stored a token")
	}
}

func TestSpotifyCallbackWithoutStateDoesNothing(t *testing.T) {
	f := newSpotifyFixture(t)
	rec := serve(f.handler, request(http.MethodGet, "/spotify/callback?code="+spotifyTestCode, owner))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-invalid-state" {
		t.Fatalf("no state = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if exchange, _ := f.fake.counts(); exchange != 0 {
		t.Fatal("a callback without state reached the token endpoint")
	}
}

func TestSpotifyVerifyAndDisconnectKeepPairingSeparate(t *testing.T) {
	f := newSpotifyFixture(t)
	f.writePairing(t, "paired")
	state, binding := f.startConnect(t)
	f.callback(state, binding, "code="+spotifyTestCode)

	rec := f.post("/spotify/verify")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-verified" {
		t.Fatalf("verify = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, refresh := f.fake.counts(); refresh == 0 {
		t.Fatal("verify did not refresh the token")
	}

	rec = f.post("/spotify/disconnect")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/spotify?flash=spotify-disconnected" {
		t.Fatalf("disconnect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := f.secrets.Get(spotify.SecretName); ok {
		t.Fatal("disconnect left the refresh token stored")
	}
	page := f.get("/spotify").Body.String()
	if !strings.Contains(page, "Paired as Clanka") {
		t.Error("disconnecting the Web API changed the Connect pairing status")
	}
}

func TestHealthShowsSpotifyPairingAndWebAPIAuthSeparately(t *testing.T) {
	f := newSpotifyFixture(t)
	f.writePairing(t, "paired")
	state, binding := f.startConnect(t)
	f.callback(state, binding, "code="+spotifyTestCode)
	rec := f.get("/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Connect pairing", "Web API auth", "Paired as Clanka", "connected"} {
		if !strings.Contains(body, want) {
			t.Errorf("health lacks %q", want)
		}
	}
}
