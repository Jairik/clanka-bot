package spotify_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"gopkg.in/yaml.v3"
)

func TestDefaultConfigValidates(t *testing.T) {
	cfg := spotify.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig.Validate: %v", err)
	}
	if cfg.Enabled() {
		t.Fatal("DefaultConfig is enabled, want the Web API off until an app is registered")
	}
	if cfg.DeviceName != spotify.DefaultDeviceName {
		t.Fatalf("device name = %q", cfg.DeviceName)
	}
	if got := cfg.StaleAfter.Duration(); got != spotify.DefaultStaleAfter {
		t.Fatalf("stale after = %s", got)
	}
}

func TestConfigValidatesTheRedirectURI(t *testing.T) {
	base := spotify.DefaultConfig()
	base.ClientID = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name     string
		redirect string
		wantErr  string
	}{
		{"tailnet https", "https://pi.tailnet.ts.net/spotify/callback", ""},
		{"loopback ipv4", "http://127.0.0.1:8082/spotify/callback", ""},
		{"loopback ipv6", "http://[::1]:8082/spotify/callback", ""},
		{"plain http is rejected", "http://pi.tailnet.ts.net/spotify/callback", "HTTPS"},
		{"localhost is rejected", "http://localhost:8082/spotify/callback", "HTTPS"},
		{"wrong path", "https://pi.tailnet.ts.net/oauth/callback", "/spotify/callback"},
		{"query is rejected", "https://pi.tailnet.ts.net/spotify/callback?x=1", "query"},
		{"fragment is rejected", "https://pi.tailnet.ts.net/spotify/callback#x", "query or fragment"},
		{"credentials are rejected", "https://user:pass@pi.tailnet.ts.net/spotify/callback", "credentials"},
		{"relative is rejected", "/spotify/callback", "absolute"},
		{"unsupported scheme", "ftp://pi.tailnet.ts.net/spotify/callback", "HTTPS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.RedirectURI = tt.redirect
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigRequiresTheClientIDAndRedirectTogether(t *testing.T) {
	cfg := spotify.DefaultConfig()
	cfg.ClientID = "abc"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a client ID without a redirect URI")
	}
	cfg = spotify.DefaultConfig()
	cfg.RedirectURI = "https://pi.tailnet.ts.net/spotify/callback"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a redirect URI without a client ID")
	}
}

func TestConfigRejectsWhitespaceInTheClientID(t *testing.T) {
	cfg := spotify.DefaultConfig()
	cfg.ClientID = "abc def"
	cfg.RedirectURI = "https://pi.tailnet.ts.net/spotify/callback"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("Validate = %v", err)
	}
}

func TestConfigDecodesDurations(t *testing.T) {
	var cfg spotify.Config
	if err := yaml.Unmarshal([]byte("stale_after: 45s\ntimeout: 2500ms\n"), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if cfg.StaleAfter.Duration() != 45*time.Second {
		t.Fatalf("stale after = %s", cfg.StaleAfter.Duration())
	}
	if cfg.Timeout.Duration() != 2500*time.Millisecond {
		t.Fatalf("timeout = %s", cfg.Timeout.Duration())
	}
}

func TestScopesAreOnlyWhatPlaybackAndPlaylistsNeed(t *testing.T) {
	want := []string{
		"user-read-playback-state",
		"user-modify-playback-state",
		"user-read-currently-playing",
		"playlist-read-private",
		"playlist-read-collaborative",
		"user-library-read",
		"user-library-modify",
	}
	if strings.Join(spotify.Scopes, " ") != strings.Join(want, " ") {
		t.Fatalf("scopes = %v, want %v", spotify.Scopes, want)
	}
	if strings.Contains(spotify.ScopeString(), "streaming") {
		t.Fatal("the Web Playback SDK streaming scope is not needed by #72/#73")
	}
}
