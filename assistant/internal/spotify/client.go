package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxAPIResponse bounds one Web API response. Playlist pages are large but
// well under this; a hostile or broken response cannot exhaust memory.
const maxAPIResponse = 4 << 20

// Client is the reusable Web API seam for the #72/#73 playback and playlist
// Tools. It attaches the in-memory bearer token, retries one 401 after a
// refresh, and returns bounded bodies. It never logs.
type Client struct {
	auth *Auth
	base string
	http *http.Client
}

// Client returns the Web API client bound to this Auth. The #72/#73 module
// calls this instead of constructing a second token manager.
func (a *Auth) Client() *Client {
	if a == nil {
		return nil
	}
	return &Client{auth: a, base: a.cfg.APIBaseURL, http: a.http}
}

// Do sends one Web API request. path must start with "/" and must not be an
// absolute URL, so a model-supplied value cannot redirect the bearer token to
// another host. body is JSON bytes or nil. The caller owns the response body.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Response, error) {
	if c == nil || c.auth == nil {
		return nil, ErrNotConfigured
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "://") {
		return nil, fmt.Errorf("spotify api: path must be relative to the API root")
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	token, err := c.auth.Token(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, method, target, token, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	// A 401 can mean the access token was revoked or expired early. Drop only
	// the token that failed and retry once with a fresh one.
	_ = resp.Body.Close()
	c.auth.Invalidate(token)
	token, err = c.auth.Token(ctx)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, method, target, token, body)
}

// send performs one request with the given bearer token.
func (c *Client) send(ctx context.Context, method, target, token string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("spotify api: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// Get reads one bounded JSON body. A non-2xx response becomes an APIError with
// the status and Spotify's machine reason only.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.doAndRead(ctx, http.MethodGet, path, query, nil)
}

// Post sends a JSON body and reads one bounded JSON body. A 204 response is an
// empty successful body.
func (c *Client) Post(ctx context.Context, path string, query url.Values, body []byte) ([]byte, error) {
	return c.doAndRead(ctx, http.MethodPost, path, query, body)
}

// Put is the playback-control method (play, pause, volume, shuffle, repeat).
func (c *Client) Put(ctx context.Context, path string, query url.Values, body []byte) ([]byte, error) {
	return c.doAndRead(ctx, http.MethodPut, path, query, body)
}

// doAndRead closes the response and enforces the body bound.
func (c *Client) doAndRead(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	resp, err := c.Do(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return nil, fmt.Errorf("spotify api: read response: %w", err)
	}
	if len(raw) > maxAPIResponse {
		return nil, fmt.Errorf("spotify api: response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(resp.StatusCode, raw)
	}
	return raw, nil
}

// APIError is a non-2xx Web API response. Reason is Spotify's machine code
// such as NO_ACTIVE_DEVICE; the message and body are deliberately discarded.
// Restricted records only whether the message was Spotify's player
// "Restriction violated" (for example pause while already paused).
type APIError struct {
	StatusCode int
	Reason     string
	Restricted bool
}

// Error renders the status and safe reason only.
func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("spotify api: status %d (%s)", e.StatusCode, e.Reason)
	}
	return fmt.Sprintf("spotify api: status %d", e.StatusCode)
}

// newAPIError extracts the machine reason from Spotify's error envelope.
func newAPIError(status int, raw []byte) *APIError {
	var envelope struct {
		Error struct {
			Status  int    `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return &APIError{
		StatusCode: status,
		Reason:     envelope.Error.Reason,
		Restricted: strings.Contains(strings.ToLower(envelope.Error.Message), "restriction violated"),
	}
}
