// Package monster controls Monster Smart Lighting devices through the Ayla
// cloud the Monster Smart Lighting app uses.
//
// The Duo+ monitor light bar (MNW7-2027-ICW, MLB7-2027-WW) is a Wi-Fi+BLE
// device: BLE exists only to provision its Wi-Fi credentials, and every
// control goes through the account. Endpoints, the sign-in chain and the
// property names follow the MIT-licensed
// https://github.com/Aareon/monster-rgbic-ha integration, which reverse-
// engineered them from the Android app and verified them on hardware.
package monster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
)

// The two write-only secrets the module declares. They are also the .env
// bootstrap names; a stored value wins.
const (
	SecretEmail    = "MONSTER_EMAIL"
	SecretPassword = "MONSTER_PASSWORD"
)

// Monster's app constants. These identify the public mobile application, not
// the owner's account, and are the same values every install of the app uses.
const (
	applicationID = "MONSTERGEN2"
	partnerID     = "162fa71e-46d6-4cc6-9eab-db1925fdcb30"
	aylaAppID     = "RGBIC-yQ-id"
	aylaAppSecret = "RGBIC-O7v7HvMh9OjQBz8eA6tL6Pprp8U"
)

// requestTimeout bounds one HTTPS call. A page load reads the device list and
// one properties document per light, so the client timeout is per request.
const requestTimeout = 20 * time.Second

// maxBody keeps a misbehaving upstream from exhausting memory. The property
// document includes the device's scene slots, which are still far smaller.
const maxBody = 1 << 20

// Errors are sentinels so the page and Health can name a cause without
// copying an upstream body (which may include a token) into a view.
var (
	ErrCredentials   = errors.New("monster rejected the email or password")
	ErrNotFound      = errors.New("monster device not found")
	ErrUpstream      = errors.New("monster did not answer")
	ErrNotConfigured = errors.New("Monster lights need an email and password on the Secrets page")
)

// StatusError is a non-authentication HTTP failure from the Ayla API.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// Unwrap lets callers test for ErrUpstream without parsing the status.
func (e *StatusError) Unwrap() error { return ErrUpstream }

// URLs are the four hosts in the sign-in and control chain. Tests replace
// them with one local server.
type URLs struct {
	Copilot  string
	Sphere   string
	AylaUser string
	AylaAds  string
}

// DefaultURLs returns Monster's production hosts: the Copilot login, the
// partner-ticket exchange, and the US Ayla "Field" region's user and device
// APIs.
func DefaultURLs() URLs {
	return URLs{
		Copilot:  "https://api.monstergen2.bycopilot.com",
		Sphere:   "https://sphere.bycopilot.com",
		AylaUser: "https://user-field.aylanetworks.com",
		AylaAds:  "https://ads-field.aylanetworks.com",
	}
}

// Device is one light on the account, as the cloud lists it.
type Device struct {
	DSN   string
	Name  string
	Model string
}

// API is the cloud seam the dashboard and Health share. *Client is the
// production implementation; tests supply a fake.
type API interface {
	Devices(ctx context.Context, email, password string) ([]Device, error)
	Properties(ctx context.Context, email, password, dsn string) (map[string]any, error)
	Set(ctx context.Context, email, password, dsn, name string, value any) error
}

// Client is one keep-alive HTTP client plus the in-memory Ayla token. It
// authenticates lazily and refreshes five minutes before Ayla's expiry.
type Client struct {
	URLs URLs
	HTTP *http.Client
	Now  func() time.Time

	// deviceID is one stable per-process UUID for the login request. The app
	// sends an install id; a fresh process is a new install.
	deviceID string

	authMu sync.Mutex

	mu       sync.Mutex
	token    string
	tokenKey string
	expires  time.Time
}

// NewClient returns a client for Monster's production hosts.
func NewClient() *Client {
	return &Client{
		URLs:     DefaultURLs(),
		HTTP:     httpclient.New(httpclient.Options{Timeout: requestTimeout}),
		Now:      time.Now,
		deviceID: randomUUID(),
	}
}

// Devices lists every device on the account. Ayla nests each device under a
// "device" key and returns a second envelope for the list itself.
func (c *Client) Devices(ctx context.Context, email, password string) ([]Device, error) {
	var body []struct {
		Device struct {
			DSN         string `json:"dsn"`
			ProductName string `json:"product_name"`
			Model       string `json:"oem_model"`
		} `json:"device"`
	}
	if err := c.aylaJSON(ctx, email, password, "GET", "/apiv1/devices.json", nil, &body); err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(body))
	for _, item := range body {
		d := item.Device
		if d.DSN == "" {
			continue
		}
		name := strings.TrimSpace(d.ProductName)
		if name == "" {
			name = strings.TrimSpace(d.Model)
		}
		if name == "" {
			name = d.DSN
		}
		out = append(out, Device{DSN: d.DSN, Name: name, Model: d.Model})
	}
	return out, nil
}

// Properties returns the device's property map. Ayla wraps every entry in a
// "property" object; the value stays generic because firmware mixes integers,
// booleans and strings.
func (c *Client) Properties(ctx context.Context, email, password, dsn string) (map[string]any, error) {
	var body []struct {
		Property struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"property"`
	}
	path := "/apiv1/dsns/" + url.PathEscape(dsn) + "/properties.json"
	if err := c.aylaJSON(ctx, email, password, "GET", path, nil, &body); err != nil {
		return nil, err
	}
	props := make(map[string]any, len(body))
	for _, item := range body {
		if item.Property.Name != "" {
			props[item.Property.Name] = item.Property.Value
		}
	}
	return props, nil
}

// Set writes one datapoint. Ayla accepts a single property per call; the
// service writes mode before the value it selects.
func (c *Client) Set(ctx context.Context, email, password, dsn, name string, value any) error {
	path := "/apiv1/dsns/" + url.PathEscape(dsn) + "/properties/" + url.PathEscape(name) + "/datapoints.json"
	body := map[string]any{"datapoint": map[string]any{"value": value}}
	return c.aylaJSON(ctx, email, password, "POST", path, body, nil)
}

// aylaJSON performs one Ayla request, re-authenticating once after a 401.
// The token cache is keyed by email so replacing the Secrets value cannot
// keep using the previous account's session.
func (c *Client) aylaJSON(ctx context.Context, email, password, method, path string, body, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.ensureToken(ctx, email, password)
		if err != nil {
			return err
		}
		data, status, err := c.do(ctx, method, c.URLs.AylaAds+path, body, map[string]string{
			"Authorization": "auth_token " + token,
		})
		if err != nil {
			return err
		}
		switch {
		case status == http.StatusUnauthorized && attempt == 0:
			// The token expired early; drop it and retry with a fresh login.
			c.invalidate(credentialKey(email, password))
			continue
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			c.invalidate(credentialKey(email, password))
			return ErrCredentials
		case status == http.StatusNotFound:
			return ErrNotFound
		case status < 200 || status >= 300:
			return &StatusError{Code: status}
		}
		if out == nil {
			return nil
		}
		if err := decodeJSON(data, out); err != nil {
			return fmt.Errorf("%w: %v", ErrUpstream, err)
		}
		return nil
	}
	return ErrCredentials
}

// ensureToken returns a cached session or runs the app's three-step sign-in:
// Copilot login, partner ticket, then an Ayla token exchange.
func (c *Client) ensureToken(ctx context.Context, email, password string) (string, error) {
	key := credentialKey(email, password)
	c.mu.Lock()
	if c.token != "" && c.tokenKey == key && c.now().Before(c.expires) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	// One login at a time: the Dashboard page and a Health poll can overlap.
	c.authMu.Lock()
	defer c.authMu.Unlock()
	c.mu.Lock()
	if c.token != "" && c.tokenKey == key && c.now().Before(c.expires) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	token, expiry, err := c.login(ctx, email, password)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.token, c.tokenKey, c.expires = token, key, expiry
	c.mu.Unlock()
	return token, nil
}

// credentialKey identifies the account without keeping the password in the
// cache: a changed password must force a new sign-in, not reuse the old token.
func credentialKey(email, password string) string {
	sum := sha256.Sum256([]byte(password))
	return email + "\x00" + hex.EncodeToString(sum[:8])
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Client) invalidate(key string) {
	c.mu.Lock()
	if c.tokenKey == key {
		c.token, c.expires = "", time.Time{}
	}
	c.mu.Unlock()
}

// login reproduces the Monster app's authentication chain. The app's public
// application identifiers are constants; the owner's credentials never leave
// this function and are never logged.
func (c *Client) login(ctx context.Context, email, password string) (string, time.Time, error) {
	loginBody := map[string]any{
		"authenticationDetails": map[string]any{
			"applicationId": applicationID,
			"email":         email,
			"password":      password,
		},
		"deviceDetails": map[string]any{
			"applicationVersion": "1.0.0",
			"deviceId":           c.deviceID,
			"deviceModel":        "HomeAssistant",
			"deviceType":         "phone",
			"osType":             "android",
			"osVersion":          "1",
			"timezone": map[string]any{
				"currentTimeMillis": 0,
				"offsetMillis":      0,
				"timezoneId":        "UTC",
			},
		},
	}
	data, status, err := c.do(ctx, "POST", c.URLs.Copilot+"/v4/auth/login", loginBody, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest {
		return "", time.Time{}, ErrCredentials
	}
	if status < 200 || status >= 300 {
		return "", time.Time{}, &StatusError{Code: status}
	}
	var copilot struct {
		AccessToken string `json:"accessToken"`
		Token       string `json:"access_token"`
	}
	if err := decodeJSON(data, &copilot); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	copilotToken := copilot.AccessToken
	if copilotToken == "" {
		copilotToken = copilot.Token
	}
	if copilotToken == "" {
		return "", time.Time{}, ErrCredentials
	}

	// Exchange the Copilot token for a partner ticket, then the ticket for an
	// Ayla session. Both are POSTs with the same public app identifiers.
	data, status, err = c.do(ctx, "POST", c.URLs.Sphere+"/v2/partner/"+partnerID+"/acquire_ticket",
		map[string]any{"applicationId": applicationID},
		map[string]string{"Authorization": "Bearer " + copilotToken})
	if err != nil {
		return "", time.Time{}, err
	}
	if status < 200 || status >= 300 {
		return "", time.Time{}, &StatusError{Code: status}
	}
	var ticket struct {
		PartnerTicket string `json:"partnerTicket"`
	}
	if err := decodeJSON(data, &ticket); err != nil || ticket.PartnerTicket == "" {
		return "", time.Time{}, fmt.Errorf("%w: no partner ticket", ErrUpstream)
	}

	data, status, err = c.do(ctx, "POST", c.URLs.AylaUser+"/api/v1/token_sign_in", map[string]any{
		"token":      ticket.PartnerTicket,
		"app_id":     aylaAppID,
		"app_secret": aylaAppSecret,
	}, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	if status < 200 || status >= 300 {
		return "", time.Time{}, &StatusError{Code: status}
	}
	var session struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(data, &session); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if session.AccessToken == "" {
		return "", time.Time{}, ErrCredentials
	}
	ttl := time.Duration(session.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	// Refresh early so a page load cannot race the expiry.
	expiry := c.now().Add(ttl - 5*time.Minute)
	if ttl <= 10*time.Minute {
		expiry = c.now().Add(ttl / 2)
	}
	return session.AccessToken, expiry, nil
}

// do performs one request with a bounded body. It never returns an upstream
// body on a non-2xx response, so a token echoed in an error cannot reach a
// log line or the Dashboard.
func (c *Client) do(ctx context.Context, method, target string, body any, headers map[string]string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: %v", ErrUpstream, readErr)
	}
	return data, resp.StatusCode, nil
}

func decodeJSON(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

// randomUUID builds the login request's install id. A crypto/rand failure
// falls back to the all-zero UUID the app also accepts; it is not a credential.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
