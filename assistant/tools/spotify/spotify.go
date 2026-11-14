// Package spotify is the Spotify Tool domain. #71 landed its shared auth seam
// (the PKCE Web API grant, the refresh-token secret and the reusable Web API
// client). #72 adds the playback Tools on that seam: play, pause, next, volume
// and now_playing, driving the Soloist device on the Pi. #73 adds the
// playlist cache and the playlist and library Tools on the same Service.
package spotify

import (
	"context"
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
	cache   *spotify.PlaylistCache
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
// receiver device name from the spotify config, then adds the Tools. With the
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
	m.cache = spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: m.Client(), Logger: deps.Logger})
	m.service = NewService(player, deps.Focus).WithLibrary(player, m.cache, deps.Names)
	return RegisterTools(r, m.service)
}

// RegisterTools adds the playback and the playlist/library Tools for one
// Service, over the same Player and Focus claim.
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
	if err := tool.Add(r, NewNowPlaying(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewPlayPlaylist(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewShuffle(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewRepeat(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewPrevious(svc)); err != nil {
		return err
	}
	if err := tool.Add(r, NewQueueAdd(svc)); err != nil {
		return err
	}
	return tool.Add(r, NewLike(svc))
}

// Start begins the playlist cache refresh: once now, in the background, and
// every six hours. It never blocks startup and does nothing while the Web API
// is not configured; the cache then loads on the first playlist request.
func (m *Module) Start(ctx context.Context) error {
	// Completion monitoring shares the module lifetime and existing Web API client.
	if m.service != nil && m.cfg.Enabled() {
		go m.service.WatchCompletion(ctx)
	}
	if m.cache != nil && m.cfg.Enabled() {
		m.cache.Start(ctx)
	}
	return nil
}

// Close cancels the playlist refresh and waits for it to stop.
func (m *Module) Close() {
	if m.cache != nil {
		m.cache.Close()
	}
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
