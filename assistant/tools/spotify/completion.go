package spotify

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// PlayCompletion uses the shared services without claiming Music Focus over Ring.
func (m *Module) PlayCompletion(ctx context.Context, text string) error {
	if m == nil || m.service == nil {
		return errors.New("Spotify unavailable")
	}
	return m.service.playCompletion(ctx, text)
}

// playCompletion always pauses with a fresh context, including failed starts.
func (s *Service) playCompletion(ctx context.Context, text string) error {
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.player.Pause(stop)
	}()
	var err error
	if strings.HasPrefix(strings.ToLower(text), "playlist:") {
		var pl spotify.Playlist
		pl, err = s.resolvePlaylist(ctx, strings.TrimSpace(text[len("playlist:"):]))
		if err == nil {
			_, err = s.library.PlayPlaylist(ctx, spotify.PlaylistPlay{URI: pl.URI, Name: pl.Name, Shuffle: true})
		}
	} else if in, ok := extractPlayPlaylist("play " + text); ok {
		var pl spotify.Playlist
		pl, err = s.resolvePlaylist(ctx, in.Name)
		if err == nil {
			_, err = s.library.PlayPlaylist(ctx, spotify.PlaylistPlay{URI: pl.URI, Name: pl.Name, Shuffle: true})
		}
	} else {
		_, err = s.player.Play(ctx, spotify.PlayRequest{Query: text, Kind: spotify.KindTrack})
	}
	if err != nil {
		return err
	}
	// Confirm that an accepted Web API call actually started the receiver.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	state, err := s.player.NowPlaying(ctx)
	if err != nil {
		return err
	}
	if !state.Playing {
		return errors.New("Spotify did not start")
	}
	<-ctx.Done()
	return ctx.Err()
}
