// Package spotify is the Spotify Tool domain. #71 landed its shared auth seam
// (the PKCE Web API grant, the refresh-token secret and the reusable Web API
// client). #72 adds the playback Tools on that seam: play, pause, next, volume
// and now_playing, driving the librespot device on the Pi. #73 registers its
// playlist and library Tools in Register with the same Service.
package spotify

import (
	"log/slog"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the Spotify domain.
type Module struct {
	cfg     spotify.Config
	auth    *spotify.Auth
	logger  *slog.Logger
	service *Service
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

// Intents declares what music handles while it holds Focus. "stop" is included
// so a bare stop silences music once no Ring or reply holds Focus.
func (*Module) Intents() []focus.Intent { return intents() }

// Register builds the shared auth seam from the secrets service main already
// created, then adds the playback Tools. A resolver that cannot write leaves
// the Dashboard page read-only rather than creating a second secret store.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.logger = deps.Logger
	store, _ := deps.Secrets.(spotify.SecretStore)
	m.auth = spotify.NewAuth(spotify.Options{
		Config: m.cfg,
		Store:  store,
		Logger: deps.Logger,
	})
	return m.registerPlayback(r, deps)
}

// registerPlayback builds the Player against the one Web API client and the
// librespot device name from the spotify config, then adds the Tools. With the
// Web API unconfigured the Tools still register and speak "not connected".
func (m *Module) registerPlayback(r *tool.Registry, deps tool.Deps) error {
	maxVolume := func() int {
		if deps.Settings == nil {
			return 0
		}
		svc := deps.Settings()
		if svc == nil {
			return 0
		}
		return spotify.MaxVolume(svc)
	}
	player := spotify.NewPlayer(m.Client(), m.cfg.DeviceName, maxVolume)
	m.service = NewService(player, deps.Focus)
	return RegisterTools(r, m.service)
}

// RegisterTools adds the playback Tools for one Service. #73 adds its Tools
// here next to these, over the same Service and Player.
func RegisterTools(r *tool.Registry, svc *Service) error {
	if err := tool.Add(r, NewPlay(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewPause(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewNext(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewVolume(svc)); err != nil {
		return err
	}
	return tool.Add(r, NewNowPlaying(svc))
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
