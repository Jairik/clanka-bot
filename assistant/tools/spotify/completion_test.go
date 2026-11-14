package spotify

import (
	"context"
	"errors"
	api "github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"sync/atomic"
	"testing"
	"time"
)

// The fake isolates receiver refusal, name resolution and cancellation from live Spotify.
type completionPlayer struct {
	Player
	Library
	paused atomic.Bool
	played chan string
	fail   bool
}

func (p *completionPlayer) Play(_ context.Context, in api.PlayRequest) (api.Played, error) {
	p.played <- in.Query
	if p.fail {
		return api.Played{}, errors.New("refused")
	}
	return api.Played{}, nil
}
func (p *completionPlayer) Pause(context.Context) error { p.paused.Store(true); return nil }
func (p *completionPlayer) NowPlaying(context.Context) (api.NowPlaying, error) {
	return api.NowPlaying{Playing: true}, nil
}
func (p *completionPlayer) PlayPlaylist(_ context.Context, in api.PlaylistPlay) (api.Played, error) {
	if !in.Shuffle {
		return api.Played{}, errors.New("missing shuffle")
	}
	p.played <- in.URI
	return api.Played{}, nil
}

// One cached playlist is enough to exercise the production shared name matcher.
type completionLists struct{}

func (completionLists) Loaded() bool                  { return true }
func (completionLists) Refresh(context.Context) error { return nil }
func (completionLists) Names() []string               { return []string{"Running"} }
func (completionLists) Lookup(s string) (api.Playlist, bool) {
	return api.Playlist{Name: s, URI: "spotify:playlist:abc"}, s == "Running"
}
func TestCompletionPlayback(t *testing.T) {
	for _, name := range []string{"song", "playlist: running", "my running playlist"} {
		t.Run(name, func(t *testing.T) {
			p := &completionPlayer{played: make(chan string, 1)}
			svc := NewService(p, nil).WithLibrary(p, completionLists{}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- svc.playCompletion(ctx, name) }()
			select {
			case <-p.played:
			case <-time.After(time.Second):
				t.Fatal("did not play")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("did not stop")
			}
			if !p.paused.Load() {
				t.Fatal("not paused")
			}
		})
	}
	p := &completionPlayer{fail: true, played: make(chan string, 1)}
	if err := NewService(p, nil).playCompletion(t.Context(), "song"); err == nil || !p.paused.Load() {
		t.Fatal("load failure did not clean up")
	}
}
