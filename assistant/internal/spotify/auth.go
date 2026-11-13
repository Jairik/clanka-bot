package spotify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
)

const (
	// stateTTL bounds one sign-in attempt. The Spotify page asks the user to
	// start again after it expires.
	stateTTL = 10 * time.Minute
	// maxPending bounds the in-memory state map. Only one owner uses the
	// Dashboard, so a handful of attempts is plenty.
	maxPending = 16
	// tokenBodyLimit bounds a token response before it is decoded.
	tokenBodyLimit = 64 << 10
	// tokenFailureCooldown keeps a broken refresh token from being hammered by
	// every request. The manual Verify action bypasses it.
	tokenFailureCooldown = 5 * time.Second
)

var (
	// ErrNotConfigured means the app ID/redirect pair or a refresh token is
	// absent. It is a configuration state, not a transient failure.
	ErrNotConfigured = errors.New("spotify: Web API is not configured")
	// ErrInvalidState means an OAuth state is missing, expired, already used,
	// belongs to another owner, or was presented without its binding cookie.
	ErrInvalidState = errors.New("spotify: sign-in state is invalid or expired")
	// ErrNeedsReconnect means Spotify rejected the stored refresh token
	// (invalid_grant). The user must connect again.
	ErrNeedsReconnect = errors.New("spotify: the stored refresh token was rejected")
	// ErrUnavailable means Spotify could not be reached or returned a server,
	// rate-limit or malformed response. It is safe to retry later.
	ErrUnavailable = errors.New("spotify: Spotify is unavailable")
	// ErrStore means the write-only secrets service is not wired.
	ErrStore = errors.New("spotify: secret store is unavailable")
)

// TokenError is a token endpoint failure. It never contains a token, code,
// verifier or raw response body.
type TokenError struct {
	Class  string
	Status int
	Reason string
}

// Error renders the class and HTTP status only.
func (e *TokenError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("spotify: token request failed (%s, status %d)", e.Class, e.Status)
	}
	return fmt.Sprintf("spotify: token request failed (%s)", e.Class)
}

// Unwrap maps the failure to the public sentinel callers branch on.
func (e *TokenError) Unwrap() error {
	switch e.Class {
	case "invalid_grant":
		return ErrNeedsReconnect
	case "invalid_client":
		return ErrNotConfigured
	default:
		return ErrUnavailable
	}
}

// WebAPIState is the truthful Web API auth state. It is separate from Connect
// pairing: a paired Soloist receiver does not imply a Web API token, and vice versa.
type WebAPIState string

const (
	// WebAPINotConfigured means the app pair or a refresh token is missing.
	WebAPINotConfigured WebAPIState = "not_configured"
	// WebAPIConfigured means a refresh token is stored but has not been proven
	// since startup.
	WebAPIConfigured WebAPIState = "configured"
	// WebAPIConnected means the last token refresh succeeded.
	WebAPIConnected WebAPIState = "connected"
	// WebAPIReconnect means Spotify rejected the refresh token.
	WebAPIReconnect WebAPIState = "needs_reconnect"
	// WebAPIError means the last refresh failed for another reason.
	WebAPIError WebAPIState = "error"
	// WebAPIStoreUnavailable means the secrets service is not wired.
	WebAPIStoreUnavailable WebAPIState = "store_unavailable"
)

// WebAPIStatus is metadata only. It never carries a token, code, verifier or
// state value.
type WebAPIStatus struct {
	State              WebAPIState
	Label              string
	Detail             string
	ClientConfigured   bool
	RedirectConfigured bool
	RedirectURI        string
	DeviceName         string
	Set                bool
	Source             string
	Last4              string
	UpdatedAt          time.Time
	HasUpdatedAt       bool
	LastRefresh        time.Time
	HasLastRefresh     bool
	ExpiresAt          time.Time
	HasExpiresAt       bool
}

// SecretStore is the write-only secrets seam. The production implementation is
// *secrets.Service; tests may supply a fake. Values never enter a view.
type SecretStore interface {
	Get(name string) (string, bool)
	Metadata(name string) (secrets.Metadata, bool)
	Set(ctx context.Context, name, value string) (secrets.Metadata, error)
	Clear(ctx context.Context, name string) (secrets.Metadata, error)
}

// Options configures an Auth. The zero value is not useful: Config must name
// the app pair for the Web API flow, and Store must resolve the secret.
type Options struct {
	Config Config
	Store  SecretStore
	Logger *slog.Logger
	// HTTP is the client used for token and Web API requests. Nil uses a
	// client with Config.Timeout.
	HTTP *http.Client
	// Now and Rand are test seams; nil uses the real clock and crypto/rand.
	Now  func() time.Time
	Rand io.Reader
}

// BeginResult is one started sign-in. Binding is the random cookie value; the
// state and PKCE verifier stay server-side and are never rendered or logged.
type BeginResult struct {
	AuthorizeURL string
	Binding      string
}

// pending is one in-flight sign-in, keyed by the OAuth state. It is deleted
// when the callback succeeds, so a replay is rejected.
type pending struct {
	owner       string
	bindingHash [sha256.Size]byte
	verifier    string
	redirect    string
	expires     time.Time
}

// authStatus is the in-memory result of the last token operation.
type authStatus struct {
	state       WebAPIState
	detail      string
	lastRefresh time.Time
	expires     time.Time
}

// Auth owns the PKCE sign-in, the in-memory access token and the single-flight
// refresh. One instance is shared by the Dashboard and the future Spotify
// Tools, so they cannot disagree about the token or its status.
type Auth struct {
	cfg    Config
	store  SecretStore
	logger *slog.Logger
	http   *http.Client
	now    func() time.Time
	rand   io.Reader

	mu      sync.Mutex
	pending map[string]pending
	access  string
	expires time.Time
	// usedRefresh is the refresh token value that produced access. Comparing
	// it with the store detects a manual replace or clear on /secrets.
	usedRefresh string
	status      authStatus
	inflight    chan struct{}
	retryAfter  time.Time
	lastErr     error
}

// NewAuth builds the shared Web API auth seam.
func NewAuth(opts Options) *Auth {
	cfg := opts.Config.normalized()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{
			Timeout: cfg.Timeout.Duration(),
			// Spotify needs no redirects; refusing one keeps the refresh token and bearer on the fixed origin.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	random := opts.Rand
	if random == nil {
		random = rand.Reader
	}
	auth := &Auth{
		cfg:     cfg,
		store:   opts.Store,
		logger:  logger,
		http:    client,
		now:     now,
		rand:    random,
		pending: make(map[string]pending),
	}
	auth.status = auth.initialStatus()
	return auth
}

// Config returns the normalized config. It is safe to render the client ID
// and redirect URI because neither is a secret; the page chooses not to show
// the client ID.
func (a *Auth) Config() Config {
	if a == nil {
		return Config{}
	}
	return a.cfg
}

// SecureCookies reports whether the OAuth binding cookie must carry Secure.
// The public Dashboard is reached over HTTPS through tailscale serve; a
// loopback test redirect may be HTTP.
func (a *Auth) SecureCookies() bool {
	if a == nil {
		return true
	}
	return strings.HasPrefix(strings.ToLower(a.cfg.RedirectURI), "https://")
}

func (a *Auth) initialStatus() authStatus {
	if a.store == nil {
		return authStatus{state: WebAPIStoreUnavailable, detail: "The secrets service is not wired."}
	}
	if !a.cfg.Enabled() {
		return authStatus{state: WebAPINotConfigured, detail: "Set spotify.client_id and spotify.redirect_uri, then restart."}
	}
	if meta, ok := a.store.Metadata(SecretName); ok && meta.Set {
		return authStatus{state: WebAPIConfigured, detail: "A refresh token is stored; it has not been used since startup."}
	}
	return authStatus{state: WebAPINotConfigured, detail: "No refresh token is stored. Connect the Web API."}
}

// Begin starts one PKCE authorization. The state is cryptographically random,
// single-use, expires after ten minutes and is bound to the owner and to a
// separate cookie value. The CSRF protection on the starting request is the
// Dashboard's same-origin POST guard.
func (a *Auth) Begin(owner string) (BeginResult, error) {
	if a == nil || !a.cfg.Enabled() {
		return BeginResult{}, ErrNotConfigured
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return BeginResult{}, fmt.Errorf("spotify: an authenticated owner is required")
	}
	state, err := a.randomString(32)
	if err != nil {
		return BeginResult{}, err
	}
	binding, err := a.randomString(32)
	if err != nil {
		return BeginResult{}, err
	}
	verifier, err := a.randomString(48)
	if err != nil {
		return BeginResult{}, err
	}
	now := a.now()
	a.mu.Lock()
	a.prunePendingLocked(now)
	if len(a.pending) >= maxPending {
		a.evictOldestPendingLocked()
	}
	a.pending[state] = pending{
		owner:       owner,
		bindingHash: sha256.Sum256([]byte(binding)),
		verifier:    verifier,
		redirect:    a.cfg.RedirectURI,
		expires:     now.Add(stateTTL),
	}
	a.mu.Unlock()

	authorize, err := url.Parse(a.cfg.AuthorizeURL)
	if err != nil {
		return BeginResult{}, fmt.Errorf("spotify: authorize URL: %w", err)
	}
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", a.cfg.ClientID)
	query.Set("redirect_uri", a.cfg.RedirectURI)
	query.Set("state", state)
	query.Set("scope", ScopeString())
	query.Set("code_challenge_method", "S256")
	query.Set("code_challenge", challengeFor(verifier))
	authorize.RawQuery = query.Encode()
	return BeginResult{AuthorizeURL: authorize.String(), Binding: binding}, nil
}

// Complete validates and consumes one callback state, exchanges the code with
// the stored PKCE verifier, and writes the refresh token to the secrets table.
// The state is consumed before the network call, so a failed exchange cannot
// be replayed.
func (a *Auth) Complete(ctx context.Context, owner, state, binding, code string) error {
	entry, err := a.consume(owner, state, binding)
	if err != nil {
		return err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return &TokenError{Class: "missing_code"}
	}
	if a.store == nil {
		return ErrStore
	}
	grant, err := a.requestToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {entry.redirect},
		"client_id":     {a.cfg.ClientID},
		"code_verifier": {entry.verifier},
	}, true)
	if err != nil {
		if !isContextError(err) {
			a.recordFailure(err)
		}
		return err
	}
	if grant.RefreshToken == "" {
		err := &TokenError{Class: "missing_refresh_token"}
		a.recordFailure(err)
		return err
	}
	if _, err := a.store.Set(ctx, SecretName, grant.RefreshToken); err != nil {
		a.recordFailure(&TokenError{Class: "store"})
		return fmt.Errorf("spotify: store refresh token: %w", err)
	}
	a.setAccess(grant, grant.RefreshToken)
	a.logger.Info("spotify web api connected")
	return nil
}

// ConsumeState validates and burns a state for an error callback
// (error=access_denied) without exchanging a code.
func (a *Auth) ConsumeState(owner, state, binding string) error {
	_, err := a.consume(owner, state, binding)
	return err
}

// consume validates every binding, deletes a successful state, and returns the
// pending entry. A mismatched owner or cookie is rejected without burning the
// legitimate user's state; a successful callback is one-use.
func (a *Auth) consume(owner, state, binding string) (pending, error) {
	if a == nil {
		return pending{}, ErrNotConfigured
	}
	owner = strings.TrimSpace(owner)
	if state == "" || binding == "" || owner == "" {
		return pending{}, ErrInvalidState
	}
	a.mu.Lock()
	entry, ok := a.pending[state]
	if ok && a.now().After(entry.expires) {
		delete(a.pending, state)
		ok = false
	}
	if ok && entry.owner == owner {
		want := sha256.Sum256([]byte(binding))
		if subtle.ConstantTimeCompare(entry.bindingHash[:], want[:]) == 1 {
			delete(a.pending, state)
			a.mu.Unlock()
			return entry, nil
		}
	}
	a.mu.Unlock()
	return pending{}, ErrInvalidState
}

// Token returns a valid access token, refreshing at most once for concurrent
// callers. The refresh token is read from the secrets table on every refresh,
// so a Dashboard replacement is picked up without a restart.
func (a *Auth) Token(ctx context.Context) (string, error) {
	if a == nil || !a.cfg.Enabled() {
		return "", ErrNotConfigured
	}
	for {
		a.mu.Lock()
		if a.access != "" && a.now().Before(a.expires.Add(-time.Minute)) {
			token := a.access
			a.mu.Unlock()
			return token, nil
		}
		if waiting := a.inflight; waiting != nil {
			a.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if a.lastErr != nil && a.now().Before(a.retryAfter) {
			err := a.lastErr
			a.mu.Unlock()
			return "", err
		}
		done := make(chan struct{})
		a.inflight = done
		a.mu.Unlock()

		token, err := a.refresh(ctx)

		a.mu.Lock()
		a.inflight = nil
		close(done)
		a.mu.Unlock()
		return token, err
	}
}

// Refresh forces one refresh even during the failure cooldown. The Dashboard's
// Verify button uses it; it is also the seam a future Tool can call after an
// unexpected 401.
func (a *Auth) Refresh(ctx context.Context) (string, error) {
	if a == nil || !a.cfg.Enabled() {
		return "", ErrNotConfigured
	}
	a.mu.Lock()
	a.access = ""
	a.expires = time.Time{}
	a.usedRefresh = ""
	a.lastErr = nil
	a.retryAfter = time.Time{}
	a.mu.Unlock()
	return a.Token(ctx)
}

// Invalidate drops the cached access token only if it is still the one that
// failed. A concurrent successful refresh is left alone.
func (a *Auth) Invalidate(token string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.access == token {
		a.access = ""
		a.expires = time.Time{}
	}
	a.mu.Unlock()
}

// Disconnect clears the stored refresh token. It does not touch the Soloist
// receiver's cached Connect credentials; pairing is a separate grant.
func (a *Auth) Disconnect(ctx context.Context) (secrets.Metadata, error) {
	if a == nil || a.store == nil {
		return secrets.Metadata{}, ErrStore
	}
	meta, err := a.store.Clear(ctx, SecretName)
	if err != nil {
		return secrets.Metadata{}, err
	}
	a.mu.Lock()
	a.access = ""
	a.expires = time.Time{}
	a.usedRefresh = ""
	a.lastErr = nil
	a.retryAfter = time.Time{}
	a.status = a.initialStatus()
	a.mu.Unlock()
	a.logger.Info("spotify web api disconnected")
	return meta, nil
}

// Status reports the truthful Web API state. It reads the secret metadata and
// detects a manual replacement without ever returning the value.
func (a *Auth) Status() WebAPIStatus {
	if a == nil {
		return WebAPIStatus{
			State:  WebAPINotConfigured,
			Label:  "not configured",
			Detail: "Spotify is not wired in this deployment.",
		}
	}
	status := WebAPIStatus{
		ClientConfigured:   a.cfg.ClientID != "",
		RedirectConfigured: a.cfg.RedirectURI != "",
		RedirectURI:        a.cfg.RedirectURI,
		DeviceName:         a.cfg.DeviceName,
	}
	if !a.cfg.Enabled() {
		status.State = WebAPINotConfigured
		status.Label = "not configured"
		status.Detail = "Set spotify.client_id and spotify.redirect_uri, then restart."
		return status
	}
	if a.store == nil {
		status.State = WebAPIStoreUnavailable
		status.Label = "unavailable"
		status.Detail = "The secrets service is not wired."
		return status
	}
	meta, declared := a.store.Metadata(SecretName)
	if !declared || !meta.Set {
		status.State = WebAPINotConfigured
		status.Label = "not configured"
		status.Detail = "No refresh token is stored. Connect the Web API."
		return status
	}
	status.Set = true
	status.Source = string(meta.Source)
	status.Last4 = meta.Last4
	if meta.UpdatedAt != nil {
		status.UpdatedAt = meta.UpdatedAt.UTC()
		status.HasUpdatedAt = true
	}

	a.mu.Lock()
	current := a.status
	expires := a.expires
	used := a.usedRefresh
	a.mu.Unlock()

	// A manual replace or clear on /secrets changes the stored value under the
	// cached access token. Invalidate it so the page never claims a connection
	// that belongs to the old token.
	if used != "" {
		if value, ok := a.store.Get(SecretName); !ok || value != used {
			a.mu.Lock()
			a.access = ""
			a.expires = time.Time{}
			a.usedRefresh = ""
			a.status = authStatus{state: WebAPIConfigured, detail: "A refresh token is stored; it has not been used since it changed."}
			a.mu.Unlock()
			current = authStatus{state: WebAPIConfigured, detail: "A refresh token is stored; it has not been used since it changed."}
			expires = time.Time{}
		}
	}

	if !current.lastRefresh.IsZero() {
		status.LastRefresh = current.lastRefresh.UTC()
		status.HasLastRefresh = true
	}
	if !expires.IsZero() {
		status.ExpiresAt = expires.UTC()
		status.HasExpiresAt = true
	}
	switch current.state {
	case WebAPIConnected:
		status.State = WebAPIConnected
		status.Label = "connected"
		status.Detail = "A stored refresh token is present and the last token refresh succeeded."
	case WebAPIReconnect:
		status.State = WebAPIReconnect
		status.Label = "reconnect needed"
		status.Detail = "Spotify rejected the stored refresh token. Connect again."
	case WebAPIError:
		status.State = WebAPIError
		status.Label = "error"
		status.Detail = current.detail
	default:
		status.State = WebAPIConfigured
		status.Label = "configured, not verified"
		status.Detail = current.detail
	}
	return status
}

// refresh performs one token grant. It must be called without a.mu held.
func (a *Auth) refresh(ctx context.Context) (string, error) {
	if a.store == nil {
		return "", ErrStore
	}
	refreshToken, ok := a.store.Get(SecretName)
	refreshToken = strings.TrimSpace(refreshToken)
	if !ok || refreshToken == "" {
		a.mu.Lock()
		a.status = authStatus{state: WebAPINotConfigured, detail: "No refresh token is stored. Connect the Web API."}
		a.mu.Unlock()
		return "", ErrNotConfigured
	}
	grant, err := a.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {a.cfg.ClientID},
	}, false)
	if err != nil {
		if !isContextError(err) {
			a.recordFailure(err)
		}
		return "", err
	}
	used := refreshToken
	if grant.RefreshToken != "" && grant.RefreshToken != refreshToken {
		// Spotify may rotate the refresh token. Persist the new value before
		// the access token is handed out; a failed write is reported as a
		// status error because the old token may already be invalid.
		if _, err := a.store.Set(ctx, SecretName, grant.RefreshToken); err != nil {
			a.setAccess(grant, refreshToken)
			a.mu.Lock()
			a.status = authStatus{
				state:       WebAPIError,
				detail:      "A rotated refresh token could not be stored. Connect again before this access token expires.",
				lastRefresh: a.now(),
				expires:     grant.ExpiresAt,
			}
			a.mu.Unlock()
			a.logger.Error("spotify refresh token rotation failed", "error", err.Error())
			return grant.AccessToken, nil
		}
		used = grant.RefreshToken
	}
	a.setAccess(grant, used)
	a.logger.Info("spotify access token refreshed")
	return grant.AccessToken, nil
}

// setAccess records a successful grant. It never logs or returns the token.
func (a *Auth) setAccess(grant tokenGrant, usedRefresh string) {
	a.mu.Lock()
	a.access = grant.AccessToken
	a.expires = grant.ExpiresAt
	a.usedRefresh = usedRefresh
	a.status = authStatus{
		state:       WebAPIConnected,
		detail:      "The last token refresh succeeded.",
		lastRefresh: a.now(),
		expires:     grant.ExpiresAt,
	}
	a.lastErr = nil
	a.retryAfter = time.Time{}
	a.mu.Unlock()
}

// recordFailure stores the safe class and a cooldown. It never stores the raw
// response or any credential.
func (a *Auth) recordFailure(err error) {
	state := WebAPIError
	detail := "The last Spotify token request failed."
	delay := tokenFailureCooldown
	switch {
	case errors.Is(err, ErrNeedsReconnect):
		state = WebAPIReconnect
		detail = "Spotify rejected the stored refresh token. Connect again."
		delay = time.Minute
	case errors.Is(err, ErrNotConfigured):
		state = WebAPINotConfigured
		detail = "Spotify rejected the configured client ID. Check spotify.client_id."
		delay = time.Minute
	}
	a.mu.Lock()
	a.status = authStatus{state: state, detail: detail}
	a.lastErr = err
	a.retryAfter = a.now().Add(delay)
	a.mu.Unlock()
	a.logger.Warn("spotify token request failed", "error", err.Error())
}

// tokenGrant is one token endpoint success. It stays local to the refresh or
// exchange call.
type tokenGrant struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// tokenResponse is the subset of Spotify's token response the assistant uses.
// Error is a machine code such as invalid_grant, never a credential.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
}

// requestToken posts one form to the token endpoint. requireRefresh is true
// for the authorization-code exchange, which must return a refresh token.
func (a *Auth) requestToken(ctx context.Context, form url.Values, requireRefresh bool) (tokenGrant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenGrant{}, &TokenError{Class: "malformed"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// A canceled caller is not a Spotify failure and must not set the
			// retry cooldown or the page's error state.
			return tokenGrant{}, ctx.Err()
		}
		return tokenGrant{}, &TokenError{Class: "transport"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tokenBodyLimit+1))
	if err != nil {
		return tokenGrant{}, &TokenError{Class: "transport", Status: resp.StatusCode}
	}
	if len(raw) > tokenBodyLimit {
		return tokenGrant{}, &TokenError{Class: "malformed", Status: resp.StatusCode}
	}
	var body tokenResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return tokenGrant{}, &TokenError{Class: classifyTokenError("", resp.StatusCode), Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenGrant{}, &TokenError{Class: classifyTokenError(body.Error, resp.StatusCode), Status: resp.StatusCode, Reason: body.Error}
	}
	if body.AccessToken == "" {
		return tokenGrant{}, &TokenError{Class: "malformed", Status: resp.StatusCode}
	}
	if requireRefresh && body.RefreshToken == "" {
		return tokenGrant{}, &TokenError{Class: "missing_refresh_token", Status: resp.StatusCode}
	}
	expiresIn := body.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	if expiresIn > 24*3600 {
		// Bound a hostile or broken response so the cache cannot claim a token
		// lives for years.
		expiresIn = 24 * 3600
	}
	return tokenGrant{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresAt:    a.now().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}

// isContextError reports a caller cancellation or deadline. It is not a
// Spotify failure and must not set the retry cooldown.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classifyTokenError maps Spotify's machine code and HTTP status to the safe
// failure class used by Status.
func classifyTokenError(code string, status int) string {
	switch code {
	case "invalid_grant":
		return "invalid_grant"
	case "invalid_client":
		return "invalid_client"
	}
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= 500:
		return "server"
	default:
		return "malformed"
	}
}

// randomString returns n crypto-random bytes as unpadded base64url.
func (a *Auth) randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(a.rand, buf); err != nil {
		return "", fmt.Errorf("spotify: random source: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// challengeFor derives the S256 PKCE challenge.
func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// prunePendingLocked drops expired attempts. The caller holds a.mu.
func (a *Auth) prunePendingLocked(now time.Time) {
	for state, entry := range a.pending {
		if now.After(entry.expires) {
			delete(a.pending, state)
		}
	}
}

// evictOldestPendingLocked removes the attempt with the earliest expiry. The
// caller holds a.mu.
func (a *Auth) evictOldestPendingLocked() {
	var oldestState string
	var oldest time.Time
	for state, entry := range a.pending {
		if oldestState == "" || entry.expires.Before(oldest) {
			oldestState = state
			oldest = entry.expires
		}
	}
	if oldestState != "" {
		delete(a.pending, oldestState)
	}
}
