package spotify

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
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

// MusicDucker re-applies the local duck to a stream that opens after a duck
// has already started (for example, the track a "play" starts mid-turn while
// the wake word or a reply still holds the duck). A nil one is a no-op. The
// concrete *music.Ducker satisfies it.
type MusicDucker interface {
	Reapply(ctx context.Context) error
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

	// ducker re-applies the local duck after a new stream opens mid-duck. It
	// is nil unless main wires the music Ducker in (a setup without music stays
	// nil and the Tools still work).
	ducker MusicDucker

	mu    sync.Mutex
	claim *focus.Claim

	// The one in-memory sleep timer. The deadline lives only in this Service
	// (a Dashboard page change never rebuilds it); a restart drops it, which
	// is acceptable because a restart stops Soloist too. All fields are guarded
	// by mu. sleepGen makes a cancelled or replaced callback a no-op.
	sleepGen      uint64
	sleepDeadline time.Time
	sleepStop     oneShot
	scheduler     scheduler
	now           func() time.Time
	announce      *announce.Queue

	wake      chan struct{}
	newTicker func(d time.Duration) (<-chan time.Time, func())
}

// NewService builds the shared state. A nil registry means a shared phrase
// cannot reach music, but the Tools still work.
func NewService(player Player, registry *focus.Registry) *Service {
	return &Service{
		player:    player,
		focus:     registry,
		now:       time.Now,
		scheduler: afterFunc,
		wake:      make(chan struct{}, 1),
	}
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

// WithDucker wires the local duck so a stream that opens after a duck has
// started is lowered before the reply that follows "play". A nil ducker leaves
// the Service unchanged; the call returns the Service so construction reads as
// one expression.
func (s *Service) WithDucker(ducker MusicDucker) *Service {
	s.ducker = ducker
	return s
}

// reapplyMusic lowers a stream that appeared after the duck started. A "play",
// "resume" or "play_playlist" opens a fresh sink-input while the turn still
// holds the duck, so the reply that follows would otherwise be spoken over
// full-volume music. The new input can trail the Play call by a beat, so the
// re-apply retries once 100ms later rather than sleeping longer in the Tool
// handler and delaying the spoken reply. It is best-effort: a failure is not
// spoken and does not fail the play.
func (s *Service) reapplyMusic(ctx context.Context) {
	if s == nil || s.ducker == nil {
		return
	}
	if err := s.ducker.Reapply(ctx); err != nil {
		return
	}
	time.Sleep(reapplyRetry)
	_ = s.ducker.Reapply(context.Background())
}

// reapplyRetry is the gap between the two Reapply attempts. It is short enough
// that the spoken reply is not noticeably delayed.
const reapplyRetry = 100 * time.Millisecond

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

// dropFocus releases the Music claim. Stop and a lost device both end the
// music, so they also cancel a pending sleep timer: it must never pause
// playback after the claim is gone.
func (s *Service) dropFocus() {
	if s == nil {
		return
	}
	s.mu.Lock()
	claim := s.claim
	s.claim = nil
	s.cancelSleepLocked()
	s.mu.Unlock()
	claim.Release()
}

// wakeWatcher signals the completion watcher that a playback command occurred.
func (s *Service) wakeWatcher() {
	if s == nil || s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
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
		// A manual pause ends the sleep timer; the music keeps its claim so a
		// bare "resume" still reaches it.
		h.s.cancelSleep()
		err, text = h.s.player.Pause(ctx), "Paused."
	case focus.Resume:
		err, text = h.s.player.Resume(ctx), "Resuming."
		if err == nil {
			h.s.wakeWatcher()
		}
	case focus.Next:
		err, text = h.s.player.Next(ctx), "Next track."
		if err == nil {
			h.s.wakeWatcher()
		}
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
	case errors.Is(err, context.DeadlineExceeded):
		return "Spotify is taking too long to answer."
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

// watchActiveInterval is the polling interval while music is actively playing.
const watchActiveInterval = 2 * time.Second

// watchIdleInterval is the backed-off polling interval when music is paused or nothing is playing.
const watchIdleInterval = 30 * time.Second

// WatchCompletion detects natural playback endings without removing paused music's resume Focus.
func (s *Service) WatchCompletion(ctx context.Context) {
	tickerFn := s.newTicker
	if tickerFn == nil {
		tickerFn = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}

	interval := watchActiveInterval
	ch, stop := tickerFn(interval)
	defer func() {
		if stop != nil {
			stop()
		}
	}()

	wasPlaying := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			// A spoken command resets the interval to 2 seconds immediately.
			interval = watchActiveInterval
			stop()
			ch, stop = tickerFn(interval)
		case <-ch:
			select {
			case <-s.wake:
				// Drain any concurrent wake signal and reset interval.
				interval = watchActiveInterval
				stop()
				ch, stop = tickerFn(interval)
			default:
			}
		}

		if s.focus == nil || !s.focus.Held(focus.Music) {
			wasPlaying = false
			if interval != watchIdleInterval {
				interval = watchIdleInterval
				stop()
				ch, stop = tickerFn(interval)
			}
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		state, err := s.player.NowPlaying(probeCtx)
		cancel()

		var apiErr *spotify.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			// Rate limited: suspend probing for the Retry-After duration.
			retry := apiErr.RetryAfter
			if retry <= 0 {
				retry = watchIdleInterval
			}
			interval = retry
			stop()
			ch, stop = tickerFn(interval)
			continue
		}

		if err != nil && !errors.Is(err, spotify.ErrNothingPlaying) {
			continue
		}

		if wasPlaying && !state.Playing && s.focus.OnReleased != nil {
			s.focus.OnReleased(focus.Music)
		}
		wasPlaying = state.Playing

		nextInterval := watchIdleInterval
		if state.Playing {
			nextInterval = watchActiveInterval
		}
		if nextInterval != interval {
			interval = nextInterval
			stop()
			ch, stop = tickerFn(interval)
		}
	}
}
