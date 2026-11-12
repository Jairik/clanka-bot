package spotify_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

const (
	testOwner    = "alice@example.com"
	testCode     = "test-authorization-code"
	testRedirect = "http://127.0.0.1:8082/spotify/callback"
	testClientID = "0123456789abcdef0123456789abcdef"
)

// testClock is a concurrency-safe fake clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeOAuth is an actual HTTP token endpoint. It validates the PKCE verifier
// against the challenge the test captured from the authorize URL, so the test
// exercises the real form and hashing code, not a stub.
type fakeOAuth struct {
	t      *testing.T
	server *httptest.Server

	mu             sync.Mutex
	challenge      string
	currentRefresh string
	accessSeq      int
	exchangeCalls  int
	refreshCalls   int
	rotate         bool
	failCode       string
	failStatus     int
	tokenRedirect  string
	blockRefresh   chan struct{}
	startedRefresh chan struct{}
	api            http.HandlerFunc
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	fake := &fakeOAuth{t: t, currentRefresh: "refresh-1"}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeOAuth) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		f.mu.Lock()
		handler := f.api
		f.mu.Unlock()
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
		return
	}
	if r.URL.Path != "/token" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	redirect := f.tokenRedirect
	f.mu.Unlock()
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		f.exchangeCalls++
		challenge := f.challenge
		f.mu.Unlock()
		if r.Form.Get("client_id") != testClientID {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		if r.Form.Get("code") != testCode {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if r.Form.Get("redirect_uri") != testRedirect {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if challenge == "" || spotifyChallenge(r.Form.Get("code_verifier")) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.currentRefresh = "refresh-1"
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"access-1","token_type":"Bearer","expires_in":3600,"refresh_token":"refresh-1","scope":"user-read-playback-state"}`)
	case "refresh_token":
		f.mu.Lock()
		f.refreshCalls++
		block := f.blockRefresh
		started := f.startedRefresh
		f.mu.Unlock()
		if block != nil {
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			<-block
		}
		f.mu.Lock()
		failCode := f.failCode
		failStatus := f.failStatus
		current := f.currentRefresh
		rotate := f.rotate
		f.mu.Unlock()
		if failStatus != 0 {
			if failCode != "" {
				w.WriteHeader(failStatus)
				_, _ = io.WriteString(w, `{"error":"`+failCode+`"}`)
				return
			}
			w.WriteHeader(failStatus)
			_, _ = io.WriteString(w, `{"error":"server_error"}`)
			return
		}
		if r.Form.Get("client_id") != testClientID || r.Form.Get("refresh_token") != current {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		f.mu.Lock()
		f.accessSeq++
		seq := f.accessSeq + 1
		nextRefresh := current
		if rotate {
			nextRefresh = "refresh-" + itoa(seq)
			f.currentRefresh = nextRefresh
		}
		f.mu.Unlock()
		body := `{"access_token":"access-` + itoa(seq) + `","token_type":"Bearer","expires_in":3600`
		if rotate {
			body += `,"refresh_token":"` + nextRefresh + `"`
		}
		body += `}`
		_, _ = io.WriteString(w, body)
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
	}
}

// spotifyChallenge is the S256 transformation, repeated here so the fake
// server independently checks the verifier the client sent.
func spotifyChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func itoa(n int) string { return strconv.Itoa(n) }

// authFixture wires the real SQLite store and secrets service so persistence
// and restart behaviour are exercised, not a fake map.
type authFixture struct {
	t       *testing.T
	ctx     context.Context
	clock   *testClock
	fake    *fakeOAuth
	store   *store.Store
	secrets *secrets.Service
	logs    *lockedBuffer
	auth    *spotify.Auth
}

func newAuthFixture(t *testing.T) *authFixture {
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
	fake := newFakeOAuth(t)
	clock := newTestClock()
	logs := &lockedBuffer{}
	cfg := spotify.DefaultConfig()
	cfg.ClientID = testClientID
	cfg.RedirectURI = testRedirect
	cfg.AuthorizeURL = fake.server.URL + "/authorize"
	cfg.TokenURL = fake.server.URL + "/token"
	cfg.APIBaseURL = fake.server.URL + "/v1"
	auth := spotify.NewAuth(spotify.Options{
		Config: cfg,
		Store:  svc,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Now:    clock.Now,
	})
	return &authFixture{t: t, ctx: ctx, clock: clock, fake: fake, store: st, secrets: svc, logs: logs, auth: auth}
}

func (f *authFixture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(f.logs, nil))
}

func (f *authFixture) beginAndComplete(t *testing.T) string {
	t.Helper()
	begin, err := f.auth.Begin(testOwner)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, err := url.Parse(begin.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	state := parsed.Query().Get("state")
	f.fake.mu.Lock()
	f.fake.challenge = parsed.Query().Get("code_challenge")
	f.fake.mu.Unlock()
	if err := f.auth.Complete(f.ctx, testOwner, state, begin.Binding, testCode); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return state
}

func TestAuthorizationCodePKCEFlowStoresOnlyTheRefreshToken(t *testing.T) {
	f := newAuthFixture(t)
	begin, err := f.auth.Begin(testOwner)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, err := url.Parse(begin.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	query := parsed.Query()
	if query.Get("response_type") != "code" || query.Get("client_id") != testClientID || query.Get("redirect_uri") != testRedirect {
		t.Fatalf("authorize query = %v", query)
	}
	if query.Get("scope") != spotify.ScopeString() {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
	if query.Get("code_challenge_method") != "S256" || len(query.Get("code_challenge")) < 43 {
		t.Fatalf("PKCE parameters = %v", query)
	}
	if len(query.Get("state")) < 43 {
		t.Fatalf("state = %q", query.Get("state"))
	}
	if begin.Binding == "" || strings.Contains(begin.AuthorizeURL, begin.Binding) {
		t.Fatal("the binding cookie must be separate from the authorize URL")
	}

	f.fake.mu.Lock()
	f.fake.challenge = query.Get("code_challenge")
	f.fake.mu.Unlock()
	if err := f.auth.Complete(f.ctx, testOwner, query.Get("state"), begin.Binding, testCode); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stored, ok := f.secrets.Get(spotify.SecretName)
	if !ok || stored != "refresh-1" {
		t.Fatalf("stored refresh token = %q, %v", stored, ok)
	}
	if _, ok := f.secrets.Get("access-1"); ok {
		t.Fatal("the access token was stored; only the refresh token may be persisted")
	}
	status := f.auth.Status()
	if status.State != spotify.WebAPIConnected || status.Source != string(secrets.SourceStore) {
		t.Fatalf("status = %+v", status)
	}

	// The cached access token is used without another token request.
	before := f.fake.refreshCalls
	token, err := f.auth.Token(f.ctx)
	if err != nil || token != "access-1" {
		t.Fatalf("Token = %q, %v", token, err)
	}
	if f.fake.refreshCalls != before {
		t.Fatal("Token refreshed while the cached access token was still valid")
	}

	// Past expiry it refreshes and persists a rotated refresh token.
	f.fake.mu.Lock()
	f.fake.rotate = true
	f.fake.mu.Unlock()
	f.clock.Advance(2 * time.Hour)
	token, err = f.auth.Token(f.ctx)
	if err != nil || token != "access-2" {
		t.Fatalf("Token after expiry = %q, %v", token, err)
	}
	if stored, _ := f.secrets.Get(spotify.SecretName); stored != "refresh-2" {
		t.Fatalf("rotated refresh token = %q", stored)
	}
	if status := f.auth.Status(); status.State != spotify.WebAPIConnected {
		t.Fatalf("status after refresh = %+v", status)
	}

	// A restart builds a new Auth over the same secrets table. It must use the
	// persisted rotated token, not an in-memory value.
	restarted := spotify.NewAuth(spotify.Options{
		Config: f.auth.Config(),
		Store:  f.secrets,
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
		Now:    f.clock.Now,
	})
	if status := restarted.Status(); status.State != spotify.WebAPIConfigured {
		t.Fatalf("restart status = %+v, want configured but unverified", status)
	}
	token, err = restarted.Token(f.ctx)
	if err != nil || token != "access-3" {
		t.Fatalf("restart Token = %q, %v", token, err)
	}
	if stored, _ := f.secrets.Get(spotify.SecretName); stored != "refresh-3" {
		t.Fatalf("restart rotated refresh token = %q", stored)
	}

	for _, secret := range []string{"access-1", "access-2", "access-3", "refresh-1", "refresh-2", "refresh-3", testCode, query.Get("state")} {
		if strings.Contains(f.logs.String(), secret) {
			t.Fatalf("log contains credential-like value %q:\n%s", secret, f.logs.String())
		}
	}
}

func TestCallbackStateIsOwnerAndCookieBoundAndOneUse(t *testing.T) {
	f := newAuthFixture(t)
	begin, err := f.auth.Begin(testOwner)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, _ := url.Parse(begin.AuthorizeURL)
	state := parsed.Query().Get("state")

	if err := f.auth.Complete(f.ctx, testOwner, state, "wrong-binding", testCode); !errors.Is(err, spotify.ErrInvalidState) {
		t.Fatalf("wrong binding = %v", err)
	}
	if err := f.auth.Complete(f.ctx, "mallory@example.com", state, begin.Binding, testCode); !errors.Is(err, spotify.ErrInvalidState) {
		t.Fatalf("wrong owner = %v", err)
	}
	if f.fake.exchangeCalls != 0 {
		t.Fatal("a rejected state reached the token endpoint")
	}

	f.fake.mu.Lock()
	f.fake.challenge = parsed.Query().Get("code_challenge")
	f.fake.mu.Unlock()
	if err := f.auth.Complete(f.ctx, testOwner, state, begin.Binding, testCode); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if err := f.auth.Complete(f.ctx, testOwner, state, begin.Binding, testCode); !errors.Is(err, spotify.ErrInvalidState) {
		t.Fatalf("replay = %v", err)
	}
	if f.fake.exchangeCalls != 1 {
		t.Fatalf("exchange calls = %d, want one", f.fake.exchangeCalls)
	}
}

func TestCallbackStateExpires(t *testing.T) {
	f := newAuthFixture(t)
	begin, err := f.auth.Begin(testOwner)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, _ := url.Parse(begin.AuthorizeURL)
	f.clock.Advance(11 * time.Minute)
	if err := f.auth.Complete(f.ctx, testOwner, parsed.Query().Get("state"), begin.Binding, testCode); !errors.Is(err, spotify.ErrInvalidState) {
		t.Fatalf("expired state = %v", err)
	}
}

func TestConsumeStateBurnsAnErrorCallback(t *testing.T) {
	f := newAuthFixture(t)
	begin, err := f.auth.Begin(testOwner)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	parsed, _ := url.Parse(begin.AuthorizeURL)
	state := parsed.Query().Get("state")
	if err := f.auth.ConsumeState(testOwner, state, begin.Binding); err != nil {
		t.Fatalf("ConsumeState: %v", err)
	}
	if err := f.auth.ConsumeState(testOwner, state, begin.Binding); !errors.Is(err, spotify.ErrInvalidState) {
		t.Fatalf("replayed ConsumeState = %v", err)
	}
}

func TestBeginRequiresAConfiguredApp(t *testing.T) {
	auth := spotify.NewAuth(spotify.Options{Config: spotify.DefaultConfig()})
	if _, err := auth.Begin(testOwner); !errors.Is(err, spotify.ErrNotConfigured) {
		t.Fatalf("Begin = %v", err)
	}
	if _, err := auth.Token(context.Background()); !errors.Is(err, spotify.ErrNotConfigured) {
		t.Fatalf("Token = %v", err)
	}
}

func TestConcurrentTokenCallsShareOneRefresh(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	f.clock.Advance(2 * time.Hour)

	block := make(chan struct{})
	started := make(chan struct{}, 1)
	f.fake.mu.Lock()
	f.fake.blockRefresh = block
	f.fake.startedRefresh = started
	before := f.fake.refreshCalls
	f.fake.mu.Unlock()

	const callers = 10
	results := make(chan string, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			token, err := f.auth.Token(f.ctx)
			if err != nil {
				errs <- err
				return
			}
			results <- token
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("no refresh started")
	}
	// Let every caller reach the in-flight wait before the refresh completes.
	time.Sleep(30 * time.Millisecond)
	close(block)
	for i := 0; i < callers; i++ {
		select {
		case token := <-results:
			if token != "access-2" {
				t.Fatalf("token = %q", token)
			}
		case err := <-errs:
			t.Fatalf("Token: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("Token callers did not finish")
		}
	}
	f.fake.mu.Lock()
	calls := f.fake.refreshCalls - before
	f.fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want one shared refresh", calls)
	}
}

func TestInvalidGrantMarksReconnectAndCoolsDown(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	f.fake.mu.Lock()
	f.fake.failCode = "invalid_grant"
	f.fake.failStatus = http.StatusBadRequest
	before := f.fake.refreshCalls
	f.fake.mu.Unlock()
	f.clock.Advance(2 * time.Hour)

	if _, err := f.auth.Token(f.ctx); !errors.Is(err, spotify.ErrNeedsReconnect) {
		t.Fatalf("Token = %v", err)
	}
	if status := f.auth.Status(); status.State != spotify.WebAPIReconnect {
		t.Fatalf("status = %+v", status)
	}
	if _, err := f.auth.Token(f.ctx); !errors.Is(err, spotify.ErrNeedsReconnect) {
		t.Fatalf("cooldown Token = %v", err)
	}
	f.fake.mu.Lock()
	calls := f.fake.refreshCalls - before
	f.fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh calls during cooldown = %d, want one", calls)
	}

	// The manual Verify path bypasses the cooldown and tries again.
	if _, err := f.auth.Refresh(f.ctx); !errors.Is(err, spotify.ErrNeedsReconnect) {
		t.Fatalf("Refresh = %v", err)
	}
	f.fake.mu.Lock()
	calls = f.fake.refreshCalls - before
	f.fake.mu.Unlock()
	if calls != 2 {
		t.Fatalf("refresh calls after manual verify = %d, want two", calls)
	}
}

func TestServerErrorIsUnavailableAndDoesNotLeakTheBody(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	f.fake.mu.Lock()
	f.fake.failStatus = http.StatusInternalServerError
	f.fake.mu.Unlock()
	f.clock.Advance(2 * time.Hour)
	_, err := f.auth.Token(f.ctx)
	if !errors.Is(err, spotify.ErrUnavailable) {
		t.Fatalf("Token = %v", err)
	}
	if status := f.auth.Status(); status.State != spotify.WebAPIError {
		t.Fatalf("status = %+v", status)
	}
	if strings.Contains(f.logs.String(), "server_error") {
		t.Fatal("the token response body was logged")
	}
}

func TestManualReplacementInvalidatesTheCachedAccessToken(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	if _, err := f.auth.Token(f.ctx); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if _, err := f.secrets.Set(f.ctx, spotify.SecretName, "manual-refresh-token-abcdefghijkl"); err != nil {
		t.Fatalf("secrets.Set: %v", err)
	}
	status := f.auth.Status()
	if status.State != spotify.WebAPIConfigured {
		t.Fatalf("status after manual replacement = %+v", status)
	}
}

func TestDisconnectClearsOnlyTheWebAPIToken(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	meta, err := f.auth.Disconnect(f.ctx)
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if meta.Set {
		t.Fatalf("metadata after disconnect = %+v", meta)
	}
	if _, ok := f.secrets.Get(spotify.SecretName); ok {
		t.Fatal("Disconnect left the refresh token stored")
	}
	if status := f.auth.Status(); status.State != spotify.WebAPINotConfigured {
		t.Fatalf("status = %+v", status)
	}
}

// lockedBuffer collects test logs from the auth goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCanceledRefreshIsNotReportedAsASpotifyFailure(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	f.clock.Advance(2 * time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.auth.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token = %v, want context.Canceled", err)
	}
	if status := f.auth.Status(); status.State != spotify.WebAPIConnected {
		t.Fatalf("status = %+v, want the last successful state", status)
	}
}

func TestTokenRefreshDoesNotFollowRedirects(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()
	f.fake.mu.Lock()
	f.fake.tokenRedirect = target.URL + "/token"
	f.fake.mu.Unlock()
	if _, err := f.auth.Refresh(f.ctx); err == nil {
		t.Fatal("Refresh followed a token-endpoint redirect")
	}
	if targetHits.Load() != 0 {
		t.Fatal("the redirect target received the refresh token")
	}
	if strings.Contains(f.logs.String(), "refresh-1") {
		t.Fatal("the token failure log leaked the refresh token")
	}
}
