package github

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
)

const (
	// apiVersion pins the REST API version the responses are written against.
	// The 2026-03-10 version is the current documented version and is accepted
	// by api.github.com; a GitHub Enterprise Server must support it too.
	apiVersion = "2026-03-10"
	// acceptJSON asks GitHub for JSON rather than its default HTML error page.
	acceptJSON = "application/vnd.github+json"
	// userAgent identifies this internal caller; GitHub requires one.
	userAgent = "clanka-bot/1.0 (+https://github.com/Jairik/clanka-bot)"

	// maxBodyBytes bounds one response body. Twenty bounded list items are
	// well below this; a pathological page is refused instead of buffered.
	maxBodyBytes = 1 << 20
	// maxListPages bounds pagination. A voice answer never needs more than two
	// pages of ten; stopping early also bounds prompt size and upstream work.
	maxListPages = 2
	// listPageSize is the per_page value for the list endpoints. The result
	// count is capped at maxResultLimit, so this is also a prompt-size bound.
	listPageSize = 10

	// Bounds for the text copied into a Tool result. GitHub text is untrusted
	// facts for the model, and a short bound keeps one item from swallowing the
	// Agent request.
	maxTitleRunes  = 200
	maxRepoRunes   = 140
	maxAuthorRunes = 80
	maxURLRunes    = 512
	maxReasonRunes = 60
	maxTypeRunes   = 40
	maxBodyRunes   = 4000
)

// Provider-contract errors. They carry no token and no response body.
var (
	// errMissingToken means the declared GITHUB_TOKEN secret has no value.
	errMissingToken = errors.New("github: GITHUB_TOKEN is not configured")
	// errMissingNotificationsToken means the optional classic-token secret has
	// no value. The notifications Tool stops here: it never substitutes the
	// fine-grained GITHUB_TOKEN, so that token is never sent to /notifications.
	errMissingNotificationsToken = errors.New("github: GITHUB_NOTIFICATIONS_TOKEN is not configured")
	// errTokenRejected means GitHub answered 401 for the stored token.
	errTokenRejected = errors.New("github: GitHub rejected the token")
	// errForbidden means GitHub refused the request, usually a permission gap.
	errForbidden = errors.New("github: GitHub refused the request")
	// errNotFound means the repository or endpoint is not visible to this token.
	errNotFound = errors.New("github: GitHub could not find the resource")
	// errRateLimited means the response reports an exhausted rate limit.
	errRateLimited = errors.New("github: GitHub rate limit reached")
	// errNotificationsClassicOnly is the verified provider contract: GitHub's
	// notifications endpoints only support a classic personal access token.
	// Fine-grained PATs are not accepted for that API, so a refusal means the
	// stored token is not classic or lacks the notifications scope, and the Tool
	// says exactly that.
	errNotificationsClassicOnly = errors.New("github: notifications endpoints require a classic personal access token")
)

// apiError is one non-2xx response. It deliberately carries only the status and
// the rate-limit flag: the response body may describe private content.
type apiError struct {
	Status      int
	RateLimited bool
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github: API returned %d", e.Status)
}

// transportError wraps a failed round trip. A failure after a POST may have
// created the resource, so the caller must not retry it and must tell the user
// the outcome is unknown.
type transportError struct {
	Method string
	Err    error
}

func (e *transportError) Error() string {
	return "github: " + e.Method + " request failed: " + e.Err.Error()
}
func (e *transportError) Unwrap() error { return e.Err }

// statusError keeps the definitive HTTP status attached to one of the safe
// provider-contract sentinels. The sentinel still drives errors.Is and the
// spoken reply; the status exists only for the local log line. Without it a
// 401/403/404/429 was indistinguishable from a transport failure, because
// mapping the response to a sentinel discarded the *apiError (issue #68).
type statusError struct {
	sentinel error
	Status   int
}

func (e *statusError) Error() string { return e.sentinel.Error() }
func (e *statusError) Unwrap() error { return e.sentinel }

// mapped pairs one safe sentinel with the status that produced it.
func mapped(sentinel error, status int) error {
	return &statusError{sentinel: sentinel, Status: status}
}

// requestNotSent reports whether the round trip failed before any part of the
// request could be delivered: DNS resolution, the TCP connect, or certificate
// verification. The POST definitely did not reach GitHub in that case, so the
// Tool can say "not sent" instead of leaving the outcome unknown. Every other
// failure stays ambiguous on purpose; a connection can fail after the request
// bytes left the Pi.
func requestNotSent(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return true
	}
	return false
}

// failureKind names the transport phase or response class for one local log
// line. It carries no address, token or body: only a stable word, so a
// status=0 line can be diagnosed without reproducing the failure.
func failureKind(err error) string {
	var withStatus *statusError
	if errors.As(err, &withStatus) {
		return "http"
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return "http"
	}
	switch {
	case errors.Is(err, errMissingToken), errors.Is(err, errMissingNotificationsToken):
		return "config"
	case errors.Is(err, errTokenRejected):
		return "auth"
	case errors.Is(err, errRateLimited):
		return "rate-limit"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial":
			return "connect"
		case "remote error":
			return "tls"
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "other"
}

// credential is one secret resolver plus the error returned when it is unset.
// Each request names the credential it uses; there is no fallback between them.
type credential struct {
	get     func() (string, bool)
	missing error
}

// client is one reusable GitHub REST caller. It owns one keep-alive HTTP client
// and resolves the token per request, so a Dashboard secret rotation applies to
// the next call without a restart. The fine-grained token serves issues, search
// and health; the classic token serves only the notifications endpoints.
type client struct {
	baseURL string
	http    *http.Client
	token   credential
	classic credential
}

// newClient builds the module's one GitHub client.
func newClient(baseURL string, timeout time.Duration, token, classic func() (string, bool)) *client {
	// The configured request timeout is the one honest bound for the call. The
	// connect phases must not be shorter than it: a cold DNS lookup through
	// Tailscale MagicDNS measured just over 2 s, and the HTTP helper's 2 s dial
	// budget (which includes DNS resolution) cut it off at exactly 2 s. That
	// surfaced as a transport failure ~2 s after the Confirmation, with the
	// ambiguous "couldn't confirm" reply and no issue created (issue #68).
	httpClient := httpclient.New(httpclient.Options{
		Timeout:             timeout,
		DialTimeout:         timeout,
		TLSHandshakeTimeout: timeout,
	})
	// These endpoints never need a redirect, and not following one keeps the bearer token on the configured origin.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
		token:   credential{get: token, missing: errMissingToken},
		classic: credential{get: classic, missing: errMissingNotificationsToken},
	}
}

// do sends one request and decodes a bounded JSON response. It never retries:
// a retry after a POST timeout could create a duplicate issue, and a retry on a
// read would still spend the user's rate limit twice. It also never includes
// the response body in an error.
func (c *client) do(ctx context.Context, cred credential, method, path string, query url.Values, body, out any) (*http.Response, error) {
	token, ok := cred.get()
	if !ok {
		return nil, cred.missing
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("github: encode request: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &transportError{Method: method, Err: err}
	}
	defer resp.Body.Close()

	// Read one byte past the cap so an oversize page is detected rather than
	// silently truncated into invalid JSON.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return resp, &transportError{Method: method, Err: err}
	}
	if len(raw) > maxBodyBytes {
		return resp, fmt.Errorf("github: response exceeds %d bytes", maxBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, &apiError{
			Status:      resp.StatusCode,
			RateLimited: rateLimited(resp),
		}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("github: invalid JSON: %w", err)
		}
	}
	return resp, nil
}

// rateLimited reports an exhausted primary or secondary rate limit from the
// documented headers. The body is not inspected.
func rateLimited(resp *http.Response) bool {
	if remaining := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")); remaining == "0" {
		return true
	}
	if strings.TrimSpace(resp.Header.Get("Retry-After")) != "" {
		return true
	}
	return false
}

// Login checks the token against GET /user and returns the authenticated login.
// Health uses it; the list Tools use the @me search qualifier instead and do
// not need an identity request of their own.
func (c *client) Login(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if _, err := c.do(ctx, c.token, http.MethodGet, "/user", nil, nil, &user); err != nil {
		return "", authError(err)
	}
	if strings.TrimSpace(user.Login) == "" {
		return "", errors.New("github: /user returned no login")
	}
	return user.Login, nil
}

// issue is the subset of the create response the Tool returns.
type issue struct {
	Repo   string
	Number int
	Title  string
	URL    string
}

// createIssue posts one issue. It sends the request exactly once: a transport
// failure after the POST may have created the issue, so a retry could duplicate
// it. The caller maps that ambiguity to a clear spoken line.
func (c *client) createIssue(ctx context.Context, repo, title, body string) (issue, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return issue{}, fmt.Errorf("github: invalid repository %q", repo)
	}
	request := struct {
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
	}{Title: title, Body: body}
	var response struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues"
	if _, err := c.do(ctx, c.token, http.MethodPost, path, nil, request, &response); err != nil {
		return issue{}, createError(err)
	}
	return issue{
		Repo:   owner + "/" + name,
		Number: response.Number,
		Title:  truncate(response.Title, maxTitleRunes),
		URL:    truncate(response.HTMLURL, maxURLRunes),
	}, nil
}

// reviewRequestedPRs asks the search API for open pull requests whose review
// the authenticated user was asked for. review-requested:@me is the "waiting on
// me" semantics; it is deliberately not a list of every open pull request. The
// search endpoint supports fine-grained PATs without an extra permission.
func (c *client) reviewRequestedPRs(ctx context.Context, repo string, limit int) (total int, prs []PullRequest, err error) {
	query := "is:open is:pr review-requested:@me"
	if repo != "" {
		query += " repo:" + repo
	}
	next := url.Values{}
	next.Set("q", query)
	next.Set("sort", "updated")
	next.Set("order", "desc")
	next.Set("per_page", strconv.Itoa(listPageSize))

	for page := 1; page <= maxListPages && len(prs) < limit; page++ {
		next.Set("page", strconv.Itoa(page))
		var response struct {
			TotalCount int `json:"total_count"`
			Items      []struct {
				Number        int    `json:"number"`
				Title         string `json:"title"`
				HTMLURL       string `json:"html_url"`
				RepositoryURL string `json:"repository_url"`
				UpdatedAt     string `json:"updated_at"`
				Draft         bool   `json:"draft"`
				User          struct {
					Login string `json:"login"`
				} `json:"user"`
			} `json:"items"`
		}
		resp, err := c.do(ctx, c.token, http.MethodGet, "/search/issues", next, nil, &response)
		if err != nil {
			return 0, nil, searchError(err)
		}
		if page == 1 {
			total = response.TotalCount
		}
		for _, item := range response.Items {
			if len(prs) == limit {
				break
			}
			prs = append(prs, PullRequest{
				Repo:      truncate(repoFromAPIURL(item.RepositoryURL), maxRepoRunes),
				Number:    item.Number,
				Title:     truncate(collapse(item.Title), maxTitleRunes),
				URL:       truncate(strings.TrimSpace(item.HTMLURL), maxURLRunes),
				Author:    truncate(item.User.Login, maxAuthorRunes),
				UpdatedAt: strings.TrimSpace(item.UpdatedAt),
				Draft:     item.Draft,
			})
		}
		if !hasNextPage(resp) {
			break
		}
	}
	if prs == nil {
		prs = []PullRequest{}
	}
	return total, prs, nil
}

// unreadNotifications lists unread notification threads with the classic token
// only (never the fine-grained one). The notifications REST API only supports a
// classic personal access token (verified against the current official docs); a
// 403/404/410 is mapped to the explicit contract error rather than a generic
// failure. The call is read-only: this Tool never marks anything as read.
func (c *client) unreadNotifications(ctx context.Context, limit int) ([]Notification, error) {
	next := url.Values{}
	next.Set("all", "false")
	next.Set("per_page", strconv.Itoa(listPageSize))

	var notifications []Notification
	for page := 1; page <= maxListPages && len(notifications) < limit; page++ {
		next.Set("page", strconv.Itoa(page))
		var response []struct {
			Reason    string `json:"reason"`
			Unread    bool   `json:"unread"`
			UpdatedAt string `json:"updated_at"`
			Subject   struct {
				Title string `json:"title"`
				Type  string `json:"type"`
			} `json:"subject"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		resp, err := c.do(ctx, c.classic, http.MethodGet, "/notifications", next, nil, &response)
		if err != nil {
			return nil, notificationsError(err)
		}
		for _, item := range response {
			if len(notifications) == limit {
				break
			}
			notifications = append(notifications, Notification{
				Repo:      truncate(item.Repository.FullName, maxRepoRunes),
				Title:     truncate(collapse(item.Subject.Title), maxTitleRunes),
				Reason:    truncate(item.Reason, maxReasonRunes),
				Type:      truncate(item.Subject.Type, maxTypeRunes),
				UpdatedAt: strings.TrimSpace(item.UpdatedAt),
			})
		}
		if !hasNextPage(resp) {
			break
		}
	}
	if notifications == nil {
		notifications = []Notification{}
	}
	return notifications, nil
}

// authError maps GET /user's failures for the health check.
func authError(err error) error {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.RateLimited:
			return mapped(errRateLimited, apiErr.Status)
		case apiErr.Status == http.StatusUnauthorized:
			return mapped(errTokenRejected, apiErr.Status)
		case apiErr.Status == http.StatusForbidden:
			return mapped(errForbidden, apiErr.Status)
		}
	}
	return err
}

// createError maps an issue-create response to a safe error. The create call
// itself is never retried; a 5xx or transport failure remains ambiguous and the
// handler turns it into the no-retry line.
func createError(err error) error {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.RateLimited:
			return mapped(errRateLimited, apiErr.Status)
		case apiErr.Status == http.StatusUnauthorized:
			return mapped(errTokenRejected, apiErr.Status)
		case apiErr.Status == http.StatusForbidden:
			return mapped(errForbidden, apiErr.Status)
		case apiErr.Status == http.StatusNotFound:
			return mapped(errNotFound, apiErr.Status)
		}
	}
	return err
}

// searchError maps the search endpoint's failures to the safe errors the Tool
// renders. A 422 is a query validation failure, not a private-content leak.
func searchError(err error) error {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.RateLimited:
			return mapped(errRateLimited, apiErr.Status)
		case apiErr.Status == http.StatusUnauthorized:
			return mapped(errTokenRejected, apiErr.Status)
		case apiErr.Status == http.StatusForbidden:
			return mapped(errForbidden, apiErr.Status)
		case apiErr.Status == http.StatusNotFound:
			return mapped(errNotFound, apiErr.Status)
		case apiErr.Status == http.StatusUnprocessableEntity:
			return mapped(errForbidden, apiErr.Status)
		}
	}
	return err
}

// notificationsError maps the verified credential contract and the ordinary
// failures. The contract error is distinct so the Tool can name the exact
// action (a classic token with the notifications scope).
func notificationsError(err error) error {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.RateLimited:
			return mapped(errRateLimited, apiErr.Status)
		case apiErr.Status == http.StatusUnauthorized:
			return mapped(errTokenRejected, apiErr.Status)
		case apiErr.Status == http.StatusForbidden,
			apiErr.Status == http.StatusNotFound,
			apiErr.Status == http.StatusGone:
			// GitHub documents this API as classic-token only. A fine-grained
			// token (or no access) lands here, so state the contract plainly.
			return mapped(errNotificationsClassicOnly, apiErr.Status)
		}
	}
	return err
}

// hasNextPage reads the documented Link header. Following it is bounded by
// maxListPages and the caller's result limit.
func hasNextPage(resp *http.Response) bool {
	link := resp.Header.Get("Link")
	if link == "" {
		return false
	}
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			return true
		}
	}
	return false
}

// repoFromAPIURL turns https://api.github.com/repos/owner/name into owner/name.
func repoFromAPIURL(raw string) string {
	const marker = "/repos/"
	index := strings.LastIndex(raw, marker)
	if index < 0 {
		return ""
	}
	return strings.TrimSuffix(raw[index+len(marker):], "/")
}

// splitRepo validates and splits one owner/name selector. It rejects a bare
// name, an extra slash and traversal-shaped segments; every path segment is
// escaped by the caller as well.
func splitRepo(repo string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(strings.TrimSpace(repo), "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	if owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", false
	}
	return owner, name, true
}

// truncate shortens text to at most max runes, preserving valid UTF-8.
func truncate(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}

// collapse turns GitHub text into one line for the Agent prompt.
func collapse(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// probeNotifications is the notifications-token health check: one read-only
// request for a single unread thread, which proves the classic token and its
// scope without touching any thread's state.
func (c *client) probeNotifications(ctx context.Context) error {
	query := url.Values{}
	query.Set("all", "false")
	query.Set("per_page", "1")
	var ignored []json.RawMessage
	if _, err := c.do(ctx, c.classic, http.MethodGet, "/notifications", query, nil, &ignored); err != nil {
		return notificationsError(err)
	}
	return nil
}
