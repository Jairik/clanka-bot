// Package integrations is the small account registry behind the Dashboard's
// Hermes page. It does not add a second credential store: every account is a
// set of the same write-only secrets the Tool modules already resolve, and a
// per-account grant is an ordinary live setting.
package integrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// VerifyClass is the mapped outcome of one verify attempt. It reuses the
// Health-page vocabulary (#165) rather than inventing a second one: a stored
// key is either ok, rejected (a 401/403), unreachable (network or timeout), or
// not configured (no secret to test).
type VerifyClass string

const (
	// VerifyOK means the one authenticated call succeeded.
	VerifyOK VerifyClass = "ok"
	// VerifyRejected means the provider answered 401/403: the stored key is
	// wrong, expired or revoked.
	VerifyRejected VerifyClass = "rejected"
	// VerifyUnreachable means the request failed to complete: a network error,
	// a connection refused, or a deadline.
	VerifyUnreachable VerifyClass = "unreachable"
	// VerifyNotConfigured means no secret was stored, so there was nothing to
	// prove.
	VerifyNotConfigured VerifyClass = "not-configured"
)

// VerifyResult is one account's verify outcome. Reason is fixed text: it never
// echoes a secret value or an upstream response body.
type VerifyResult struct {
	Class  VerifyClass `json:"class"`
	Reason string      `json:"reason"`
}

// SecretReader resolves a declared secret from the store, then the environment.
// *secrets.Service satisfies it.
type SecretReader interface {
	Get(name string) (string, bool)
}

// SpotifyVerifier proves the Web API token by calling GET /me through the token
// manager. The existing Auth satisfies it.
type SpotifyVerifier interface {
	Verify(ctx context.Context) error
}

// MonsterVerifier reuses the existing login/Health probe. The existing
// monster.Service satisfies it.
type MonsterVerifier interface {
	Health(ctx context.Context) (configured, ok bool, detail string)
}

// Verifier proves a stored integration key works with one cheap authenticated
// call per account. It holds the secrets reader and an HTTP client for the
// accounts it verifies directly (GitHub, OpenRouter, WeatherAPI, ntfy), and
// narrow interfaces for the providers whose auth is stateful (Spotify) or whose
// probe is owned elsewhere (Monster). It never follows a user-supplied URL:
// every base URL is wired in main from the deployment's own configuration.
type Verifier struct {
	secrets        SecretReader
	http           *http.Client
	spotify        SpotifyVerifier
	monster        MonsterVerifier
	githubBase     string
	openrouterBase string
	weatherURL     string
	ntfyBase       string
	// timeout bounds each direct HTTP call. Monster reuses its own (slower)
	// login timeout, and Spotify rides the token manager's.
	timeout time.Duration
}

// VerifierOptions configure a Verifier. The base URLs are the deployment's own,
// never the request's.
type VerifierOptions struct {
	Secrets        SecretReader
	HTTP           *http.Client
	Spotify        SpotifyVerifier
	Monster        MonsterVerifier
	GitHubBase     string
	OpenRouterBase string
	WeatherURL     string
	NtfyBase       string
	Timeout        time.Duration
}

// NewVerifier builds the registry. Either seam may be nil; the matching account
// then reports not-configured rather than failing.
func NewVerifier(opts VerifierOptions) *Verifier {
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Verifier{
		secrets:        opts.Secrets,
		http:           httpClient,
		spotify:        opts.Spotify,
		monster:        opts.Monster,
		githubBase:     strings.TrimRight(opts.GitHubBase, "/"),
		openrouterBase: strings.TrimRight(opts.OpenRouterBase, "/"),
		weatherURL:     strings.TrimSpace(opts.WeatherURL),
		ntfyBase:       strings.TrimRight(opts.NtfyBase, "/"),
		timeout:        timeout,
	}
}

// Verify proves one account's stored key. The account id is one of the catalog
// ids; an unknown id is an error so the caller can answer 404.
func (v *Verifier) Verify(ctx context.Context, id string) (VerifyResult, error) {
	switch id {
	case "github":
		return v.verifyGitHub(ctx), nil
	case "openrouter":
		return v.verifyOpenRouter(ctx), nil
	case "weather":
		return v.verifyWeather(ctx), nil
	case "notify":
		return v.verifyNtfy(ctx), nil
	case "spotify":
		return v.verifySpotify(ctx), nil
	case "monster":
		return v.verifyMonster(ctx), nil
	default:
		return VerifyResult{}, fmt.Errorf("%w: %q", ErrUnknownAccount, id)
	}
}

// verifyGitHub proves the token against GET /user.
func (v *Verifier) verifyGitHub(ctx context.Context) VerifyResult {
	return v.getAuth(ctx, v.githubBase+"/user", "GITHUB_TOKEN", func(req *http.Request) {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		req.Header.Set("User-Agent", "clanka-bot/1.0 (+https://github.com/Jairik/clanka-bot)")
	})
}

// verifyOpenRouter proves the key against GET /api/v1/key. /api/v1/models is
// public and would pass with a bogus key, so the key endpoint is the one that
// proves ownership.
func (v *Verifier) verifyOpenRouter(ctx context.Context) VerifyResult {
	return v.getBearer(ctx, v.openrouterBase+"/key", "OPENROUTER_API_KEY")
}

// verifyWeather proves the key against current.json with a fixed query.
func (v *Verifier) verifyWeather(ctx context.Context) VerifyResult {
	key, have := v.secrets.Get("WEATHERAPI_API_KEY")
	if !have || strings.TrimSpace(key) == "" {
		return notConfigured()
	}
	u, err := url.Parse(v.weatherURL)
	if err != nil {
		return unreachable()
	}
	q := u.Query()
	q.Set("key", strings.TrimSpace(key))
	q.Set("q", "London")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return unreachable()
	}
	callCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	resp, err := v.http.Do(req.WithContext(callCtx))
	if err != nil {
		return unreachable()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		// WeatherAPI can answer 200 with an error object for a bad key; the
		// body is never exposed, only its shape is used to flip the class.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if bytes.Contains(body, []byte(`"error"`)) {
			return rejected()
		}
		return ok()
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return rejected()
	}
	return unreachable()
}

// verifyNtfy publishes one message to the configured topic. The publish is the
// proof: a topic or token that the server rejects comes back as a 401/403, and
// a refused connection is unreachable. It only runs because the owner pressed
// the button.
func (v *Verifier) verifyNtfy(ctx context.Context) VerifyResult {
	topic, ok := v.secrets.Get("NTFY_TOPIC")
	if !ok || strings.TrimSpace(topic) == "" {
		return notConfigured()
	}
	token, _ := v.secrets.Get("NTFY_TOKEN")
	target := v.ntfyBase + "/" + url.PathEscape(strings.TrimSpace(topic))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("Clanka verification check"))
	if err != nil {
		return unreachable()
	}
	req.Header.Set("Title", "Clanka verification")
	req.Header.Set("User-Agent", "clanka-bot")
	req.Header.Set("Content-Type", "text/plain")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	return v.doCall(ctx, req)
}

// verifySpotify proves the Web API token through the existing token manager,
// which GET /me with a current access token.
func (v *Verifier) verifySpotify(ctx context.Context) VerifyResult {
	if v.spotify == nil {
		return notConfigured()
	}
	callCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	err := v.spotify.Verify(callCtx)
	if err == nil {
		return ok()
	}
	if errors.Is(err, spotify.ErrNotConfigured) {
		return notConfigured()
	}
	if status, _ := spotify.APIStatus(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
		return rejected()
	}
	return unreachable()
}

// verifyMonster reuses the existing login/Health probe. Cold login can be slow,
// so the request context (and the probe's own timeout) is kept instead of the
// three-second bound: the page shows "checking…" while it runs.
func (v *Verifier) verifyMonster(ctx context.Context) VerifyResult {
	if v.monster == nil {
		return notConfigured()
	}
	configured, healthy, detail := v.monster.Health(ctx)
	if !configured {
		return notConfigured()
	}
	if healthy {
		return ok()
	}
	// The Monster probe returns only a friendly detail string; the two failure
	// modes are a rejected credential or an unreachable cloud. The detail is
	// never exposed, only its class is.
	if strings.Contains(detail, "rejected the email or password") {
		return rejected()
	}
	return unreachable()
}

// getBearer proves a Bearer-token account with one GET.
func (v *Verifier) getBearer(ctx context.Context, target, secret string) VerifyResult {
	return v.getAuth(ctx, target, secret, nil)
}

// getAuth proves a secret-bearing account with one GET, setting the bearer token
// and any extra headers the caller supplies.
func (v *Verifier) getAuth(ctx context.Context, target, secret string, extra func(*http.Request)) VerifyResult {
	value, ok := v.secrets.Get(secret)
	if !ok || strings.TrimSpace(value) == "" {
		return notConfigured()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return unreachable()
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(value))
	req.Header.Set("User-Agent", "clanka-bot/1.0")
	if extra != nil {
		extra(req)
	}
	return v.doCall(ctx, req)
}

// doCall runs one request inside the verify timeout and maps the outcome to a
// fixed class. Never returns the upstream body or status detail.
func (v *Verifier) doCall(ctx context.Context, req *http.Request) VerifyResult {
	callCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	resp, err := v.http.Do(req.WithContext(callCtx))
	if err != nil {
		return unreachable()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return ok()
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return rejected()
	default:
		return unreachable()
	}
}

func ok() VerifyResult            { return VerifyResult{Class: VerifyOK, Reason: "The stored key works."} }
func rejected() VerifyResult      { return VerifyResult{Class: VerifyRejected, Reason: "The stored credentials were rejected."} }
func unreachable() VerifyResult   { return VerifyResult{Class: VerifyUnreachable, Reason: "Could not reach the service."} }
func notConfigured() VerifyResult { return VerifyResult{Class: VerifyNotConfigured, Reason: "No key is configured."} }
