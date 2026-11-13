package spotify

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Player is the playback seam the Tools and the Focus holder drive.
// *spotify.Player satisfies it; #73 extends the concrete Player and adds its
// own narrow interface rather than growing this one.
type Player interface {
	Play(ctx context.Context, req spotify.PlayRequest) (spotify.Played, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Next(ctx context.Context) error
	SetVolume(ctx context.Context, percent int) (applied int, capped bool, err error)
	AdjustVolume(ctx context.Context, delta int) (applied int, capped bool, err error)
	NowPlaying(ctx context.Context) (spotify.NowPlaying, error)
	Cap() int
}

// Library is the playlist and library seam behind the #73 Tools.
// *spotify.Player satisfies it. It is a separate interface so the playback
// Player seam above keeps its #72 shape.
type Library interface {
	PlayPlaylist(ctx context.Context, req spotify.PlaylistPlay) (spotify.Played, error)
	SetShuffle(ctx context.Context, on bool) error
	SetRepeat(ctx context.Context, mode string) error
	Previous(ctx context.Context) error
	QueueAdd(ctx context.Context, query string) (spotify.Queued, error)
	LikeCurrent(ctx context.Context) (spotify.Liked, error)
}

// Playlists is the local playlist cache a spoken name resolves against.
// *spotify.PlaylistCache satisfies it.
type Playlists interface {
	Loaded() bool
	Refresh(ctx context.Context) error
	Names() []string
	Lookup(name string) (spotify.Playlist, bool)
}

// Service is the state shared by the Spotify Tools: the Player and the one
// Music Focus claim. Playing music claims Focus at the Music priority, so a
// Ring or a reply holding higher Focus receives "stop" first. "skip" and
// "pause" are the next intent, so they advance the track while music holds Focus.
type Service struct {
	player    Player
	focus     *focus.Registry
	library   Library
	playlists Playlists
	names     *namematch.Matcher

	mu    sync.Mutex
	claim *focus.Claim
}

// NewService builds the shared state. A nil registry means a shared phrase
// cannot reach music, but the Tools still work.
func NewService(player Player, registry *focus.Registry) *Service {
	return &Service{player: player, focus: registry}
}

// WithLibrary adds the playlist and library seams for the #73 Tools: the
// Player's library methods, the playlist cache and the one shared name matcher.
// A nil matcher resolves exact and normalised names only. It returns the
// Service so construction reads as one expression.
func (s *Service) WithLibrary(library Library, playlists Playlists, names *namematch.Matcher) *Service {
	if names == nil {
		names = namematch.New(nil, namematch.Config{})
	}
	s.library, s.playlists, s.names = library, playlists, names
	return s
}

// intents is what music handles while it holds Focus.
func intents() []focus.Intent {
	return []focus.Intent{focus.Pause, focus.Resume, focus.Next, focus.Stop}
}

// takeFocus claims Music priority for this Service. Paused music keeps the
// claim so a bare "resume" still reaches it; stop, or a lost device, releases.
func (s *Service) takeFocus() {
	if s == nil || s.focus == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.claim
	s.claim = s.focus.Claim(focus.Music, holder{s})
	old.Release()
}

// dropFocus releases the Music claim.
func (s *Service) dropFocus() {
	if s == nil {
		return
	}
	s.mu.Lock()
	claim := s.claim
	s.claim = nil
	s.mu.Unlock()
	claim.Release()
}

// holder is the focus.Holder for music. It is a distinct type so the Service's
// own methods are not accidentally offered as Focus intents.
type holder struct{ s *Service }

func (holder) Intents() []focus.Intent { return intents() }

// HandleIntent acts on the Pi's music. It reports false when the device is
// gone, releasing the claim, so a lower holder or the semantic path still gets
// the phrase instead of the user hearing a dead end.
func (h holder) HandleIntent(ctx context.Context, intent focus.Intent) (string, bool) {
	var err error
	text := ""
	switch intent {
	case focus.Pause:
		err, text = h.s.player.Pause(ctx), "Paused."
	case focus.Resume:
		err, text = h.s.player.Resume(ctx), "Resuming."
	case focus.Next:
		err, text = h.s.player.Next(ctx), "Next track."
	case focus.Stop:
		err, text = h.s.player.Pause(ctx), "Stopped the music."
		// Stop ends the music's claim however the pause went, so a failed
		// pause cannot leave "stop" captured by a dead Focus.
		h.s.dropFocus()
	default:
		return "", false
	}
	if err == nil {
		return text, true
	}
	if lostDevice(err) {
		h.s.dropFocus()
		return "", false
	}
	return failureText(err), true
}

// lostDevice reports that the Pi's Connect device is not playing, so the music
// Focus is stale.
func lostDevice(err error) bool {
	if errors.Is(err, spotify.ErrDeviceNotFound) {
		return true
	}
	status, reason := spotify.APIStatus(err)
	return status == http.StatusNotFound || reason == "NO_ACTIVE_DEVICE"
}

// failureText is the spoken line for a Web API failure. It never contains the
// provider's message, a token or a URL.
func failureText(err error) string {
	status, reason := spotify.APIStatus(err)
	switch {
	case errors.Is(err, spotify.ErrNotConfigured), errors.Is(err, spotify.ErrNeedsReconnect):
		return "Spotify isn't connected. Connect it on the dashboard."
	case errors.Is(err, spotify.ErrDeviceNotFound), reason == "NO_ACTIVE_DEVICE", status == http.StatusNotFound:
		return "I can't find the speaker in Spotify. Open Spotify on your phone and pick it once."
	case errors.Is(err, spotify.ErrNothingFound):
		return "I couldn't find anything to play for that."
	case errors.Is(err, spotify.ErrNothingPlaying):
		return "Nothing is playing."
	case reason == "PREMIUM_REQUIRED":
		return "Spotify needs a Premium account for that."
	case status == http.StatusTooManyRequests, errors.Is(err, spotify.ErrUnavailable):
		return "Spotify is busy. Try again in a moment."
	default:
		return "I couldn't reach Spotify."
	}
}

// toolError turns an error into the one spoken line, passing through errors
// that are already a ToolError.
func toolError(err error) error {
	var spoken *tool.ToolError
	if errors.As(err, &spoken) {
		return err
	}
	return &tool.ToolError{Say: failureText(err)}
}
