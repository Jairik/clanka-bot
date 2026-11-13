// Package spotify owns the Spotify Web API authentication seam and the
// Connect receiver's pairing status reader. The Web API refresh token is the
// only Spotify credential stored by the assistant: it lives in the write-only
// secrets table, never in config, a file or a log. Access tokens stay in
// memory and are refreshed on demand with one in-flight request.
package spotify

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// SecretName is the write-only secret that holds the Web API refresh
	// token. It is both the secrets-table key and the .env bootstrap name.
	SecretName = "SPOTIFY_REFRESH_TOKEN"

	// DefaultDeviceName is the Spotify Connect device name. It must match the
	// Soloist container's SPOTIFY_DEVICE_NAME so the page and the device list
	// agree.
	DefaultDeviceName = "Clanka"
	// DefaultStreamMatch is the music receiver's Pulse application name.
	DefaultStreamMatch = "spotify"
	// DefaultTimeout bounds one token or Web API request.
	DefaultTimeout = 10 * time.Second
	// DefaultStaleAfter is how long a receiver status heartbeat may be old
	// before the page calls it unavailable. The container writes every 10 s.
	DefaultStaleAfter = 45 * time.Second

	// redirectPath is the one callback route the Dashboard serves. The
	// configured redirect URI must end here or the callback can never arrive.
	redirectPath = "/spotify/callback"

	authorizeEndpoint = "https://accounts.spotify.com/authorize"
	tokenEndpoint     = "https://accounts.spotify.com/api/token"
	apiEndpoint       = "https://api.spotify.com/v1"
)

// Scopes is exactly what the playback (#72) and playlist/library (#73)
// tickets need. Requesting more would ask the user for access the assistant
// cannot yet use.
var Scopes = []string{
	"user-read-playback-state",
	"user-modify-playback-state",
	"user-read-currently-playing",
	"playlist-read-private",
	"playlist-read-collaborative",
	"user-library-read",
	"user-library-modify",
}

// ScopeString returns the space-separated scope parameter.
func ScopeString() string { return strings.Join(Scopes, " ") }

// Duration is a YAML-friendly time.Duration written as "45s" or "10s".
type Duration time.Duration

// Duration returns the standard library duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("spotify: invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the non-secret spotify config section. ClientID is the public app
// identifier; there is no client secret because the flow uses PKCE.
type Config struct {
	// ClientID is the Spotify app's public client ID.
	ClientID string `yaml:"client_id"`
	// RedirectURI must exactly match one entry in the Spotify app's Redirect
	// URI allowlist. HTTPS is required except for a loopback IP literal.
	RedirectURI string `yaml:"redirect_uri"`
	// PairingStatusFile is the Soloist heartbeat file mounted read-only into
	// the assistant. Empty disables the Connect pairing row.
	PairingStatusFile string `yaml:"pairing_status_file"`
	// DeviceName is the name Soloist advertises. The page shows it in the
	// pairing instructions.
	DeviceName string `yaml:"device_name"`
	// StreamMatch is a case-insensitive substring of the music receiver's
	// Pulse application name, used only to duck its stream. Soloist reports
	// "spotify"; changing it requires a restart of the assistant.
	StreamMatch string `yaml:"stream_match"`
	// StaleAfter is how old a heartbeat may be before pairing is unknown.
	StaleAfter Duration `yaml:"stale_after"`
	// Timeout bounds one token or Web API request.
	Timeout Duration `yaml:"timeout"`

	// The three endpoint fields are fixed at runtime and are test seams only.
	AuthorizeURL string `yaml:"-"`
	TokenURL     string `yaml:"-"`
	APIBaseURL   string `yaml:"-"`
}

// DefaultConfig is the shipped Spotify configuration. Empty client ID and
// redirect URI leave the Web API feature off; pairing is independent.
func DefaultConfig() Config {
	return Config{
		DeviceName:   DefaultDeviceName,
		StreamMatch:  DefaultStreamMatch,
		StaleAfter:   Duration(DefaultStaleAfter),
		Timeout:      Duration(DefaultTimeout),
		AuthorizeURL: authorizeEndpoint,
		TokenURL:     tokenEndpoint,
		APIBaseURL:   apiEndpoint,
	}
}

// normalized trims user input and fills the fixed endpoints and defaults.
func (c Config) normalized() Config {
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.RedirectURI = strings.TrimSpace(c.RedirectURI)
	c.PairingStatusFile = strings.TrimSpace(c.PairingStatusFile)
	c.DeviceName = strings.TrimSpace(c.DeviceName)
	if c.DeviceName == "" {
		c.DeviceName = DefaultDeviceName
	}
	c.StreamMatch = strings.TrimSpace(c.StreamMatch)
	if c.StreamMatch == "" {
		c.StreamMatch = DefaultStreamMatch
	}
	if c.StaleAfter.Duration() <= 0 {
		c.StaleAfter = Duration(DefaultStaleAfter)
	}
	if c.Timeout.Duration() <= 0 {
		c.Timeout = Duration(DefaultTimeout)
	}
	if c.AuthorizeURL == "" {
		c.AuthorizeURL = authorizeEndpoint
	}
	if c.TokenURL == "" {
		c.TokenURL = tokenEndpoint
	}
	if c.APIBaseURL == "" {
		c.APIBaseURL = apiEndpoint
	}
	c.APIBaseURL = strings.TrimRight(c.APIBaseURL, "/")
	return c
}

// Enabled reports whether both OAuth inputs are present.
func (c Config) Enabled() bool {
	c = c.normalized()
	return c.ClientID != "" && c.RedirectURI != ""
}

// Validate checks the config layer before startup. A half-configured app is a
// startup error, not a silently disabled button.
func (c Config) Validate() error {
	c = c.normalized()
	if c.DeviceName == "" {
		return fmt.Errorf("spotify: device_name is required")
	}
	if strings.ContainsAny(c.ClientID, " \t\r\n") {
		return fmt.Errorf("spotify: client_id must not contain whitespace")
	}
	if (c.ClientID == "") != (c.RedirectURI == "") {
		return fmt.Errorf("spotify: client_id and redirect_uri must be set together")
	}
	if c.RedirectURI != "" {
		parsed, err := url.Parse(c.RedirectURI)
		if err != nil {
			return fmt.Errorf("spotify: redirect_uri: %w", err)
		}
		if !parsed.IsAbs() || parsed.Host == "" {
			return fmt.Errorf("spotify: redirect_uri must be an absolute URL")
		}
		if parsed.User != nil {
			return fmt.Errorf("spotify: redirect_uri must not contain credentials")
		}
		if strings.ContainsAny(parsed.Host, "<> \t") {
			return fmt.Errorf("spotify: redirect_uri still contains a placeholder")
		}
		if parsed.Fragment != "" || parsed.RawQuery != "" {
			return fmt.Errorf("spotify: redirect_uri must not contain a query or fragment")
		}
		if parsed.Path != redirectPath {
			return fmt.Errorf("spotify: redirect_uri path must be %s", redirectPath)
		}
		switch parsed.Scheme {
		case "https":
		case "http":
			// Spotify allows HTTP only for the explicit loopback IP literals;
			// "localhost" is not accepted by its redirect URI validation.
			if host := parsed.Hostname(); host != "127.0.0.1" && host != "::1" {
				return fmt.Errorf("spotify: redirect_uri must use HTTPS unless it is a loopback IP literal")
			}
		default:
			return fmt.Errorf("spotify: redirect_uri must use HTTPS or loopback HTTP")
		}
	}
	if c.StaleAfter.Duration() <= 0 {
		return fmt.Errorf("spotify: stale_after must be positive")
	}
	if c.Timeout.Duration() <= 0 {
		return fmt.Errorf("spotify: timeout must be positive")
	}
	return nil
}
