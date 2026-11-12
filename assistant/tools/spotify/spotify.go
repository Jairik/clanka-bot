// Package spotify is the Spotify Tool domain. #71 lands its shared auth seam:
// the PKCE Web API grant, the refresh-token secret declaration and the
// reusable Web API client. The playback and playlist Tools themselves are
// #72/#73; they register here without a second token manager or secret.
package spotify

import (
	"log/slog"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the Spotify domain. It owns no Tools yet, so the module list still
// has one wiring point when #72 adds them.
type Module struct {
	cfg    spotify.Config
	auth   *spotify.Auth
	logger *slog.Logger
}

// NewModule builds the Spotify module with the Web API disabled until main
// binds the config.
func NewModule() *Module {
	return &Module{cfg: spotify.DefaultConfig()}
}

// Name is the module name and the secret-declaration domain.
func (*Module) Name() string { return "spotify" }

// BindConfig supplies the non-secret spotify section before registration. It
// is a binding, not a second config store: main maps the one config.Config.
func (m *Module) BindConfig(cfg spotify.Config) {
	m.cfg = cfg
}

// RequiredSecrets declares the Web API refresh token. The Secrets page lists
// it automatically, and the OAuth callback may write only this name.
func (*Module) RequiredSecrets() []string { return []string{spotify.SecretName} }

// Register builds the shared auth seam from the secrets service main already
// created. A resolver that cannot write leaves the Dashboard page read-only
// rather than creating a second secret store.
func (m *Module) Register(_ *tool.Registry, deps tool.Deps) error {
	m.logger = deps.Logger
	store, _ := deps.Secrets.(spotify.SecretStore)
	m.auth = spotify.NewAuth(spotify.Options{
		Config: m.cfg,
		Store:  store,
		Logger: deps.Logger,
	})
	return nil
}

// Auth is the one Web API auth seam. The Dashboard and the #72/#73 Tools share
// it, so a refresh cannot race another caller.
func (m *Module) Auth() *spotify.Auth {
	if m == nil {
		return nil
	}
	return m.auth
}

// Client is the reusable Web API client for the later Spotify Tools.
func (m *Module) Client() *spotify.Client {
	if m == nil || m.auth == nil {
		return nil
	}
	return m.auth.Client()
}
