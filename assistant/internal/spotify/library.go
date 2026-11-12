package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Repeat modes the Web API accepts.
const (
	RepeatOff     = "off"
	RepeatTrack   = "track"
	RepeatContext = "context"
)

// PlaylistPlay is one request to start a playlist the user owns or follows.
// URI comes from the playlist cache, never from the spoken words.
type PlaylistPlay struct {
	Name    string
	URI     string
	Shuffle bool
	Volume  *int
}

// Queued is the track a queue_add call chose.
type Queued struct {
	Name   string
	Artist string
}

// Liked is the track a like call saved.
type Liked struct {
	Track   string
	Artists string
}

// PlayPlaylist starts a playlist on the Pi device. Shuffle is only sent when
// it is asked for: a plain "play my running playlist" leaves the user's
// shuffle state as it was.
func (p *Player) PlayPlaylist(ctx context.Context, req PlaylistPlay) (Played, error) {
	if strings.TrimSpace(req.URI) == "" {
		return Played{}, ErrNothingFound
	}
	d, err := p.findDevice(ctx)
	if err != nil {
		return Played{}, err
	}
	out := Played{Name: req.Name, Kind: KindPlaylist}
	if req.Shuffle {
		if err := p.putShuffle(ctx, d, true); err != nil {
			return Played{}, err
		}
	}
	if err := p.applyStartVolume(ctx, d, req.Volume, &out); err != nil {
		return Played{}, err
	}
	body, _ := json.Marshal(map[string]any{"context_uri": req.URI})
	if _, err := p.api.Put(ctx, "/me/player/play", deviceQuery(d), body); err != nil {
		return Played{}, err
	}
	return out, nil
}

// SetShuffle turns shuffle on or off on the Pi.
func (p *Player) SetShuffle(ctx context.Context, on bool) error {
	d, err := p.findDevice(ctx)
	if err != nil {
		return err
	}
	return p.putShuffle(ctx, d, on)
}

func (p *Player) putShuffle(ctx context.Context, d device, on bool) error {
	query := deviceQuery(d)
	query.Set("state", strconv.FormatBool(on))
	_, err := p.api.Put(ctx, "/me/player/shuffle", query, nil)
	return err
}

// SetRepeat sets the repeat mode on the Pi: off, track or context. Any other
// value is rejected before a request is made.
func (p *Player) SetRepeat(ctx context.Context, mode string) error {
	switch mode {
	case RepeatOff, RepeatTrack, RepeatContext:
	default:
		return fmt.Errorf("spotify: unknown repeat mode %q", mode)
	}
	d, err := p.findDevice(ctx)
	if err != nil {
		return err
	}
	query := deviceQuery(d)
	query.Set("state", mode)
	_, err = p.api.Put(ctx, "/me/player/repeat", query, nil)
	return err
}

// Previous goes back a track on the Pi.
func (p *Player) Previous(ctx context.Context) error {
	d, err := p.findDevice(ctx)
	if err != nil {
		return err
	}
	_, err = p.api.Post(ctx, "/me/player/previous", deviceQuery(d), nil)
	return err
}

// QueueAdd searches for the best matching track and adds it to the Pi's queue.
func (p *Player) QueueAdd(ctx context.Context, query string) (Queued, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Queued{}, ErrNothingFound
	}
	d, err := p.findDevice(ctx)
	if err != nil {
		return Queued{}, err
	}
	found, err := p.search(ctx, query, KindTrack)
	if err != nil {
		return Queued{}, err
	}
	q := deviceQuery(d)
	q.Set("uri", found.URI)
	if _, err := p.api.Post(ctx, "/me/player/queue", q, nil); err != nil {
		return Queued{}, err
	}
	return Queued{Name: found.Name, Artist: found.Artist}, nil
}

// LikeCurrent saves the track that is playing to the user's library. It never
// guesses: with no current track it returns ErrNothingPlaying and saves
// nothing.
func (p *Player) LikeCurrent(ctx context.Context) (Liked, error) {
	raw, err := p.api.Get(ctx, "/me/player", nil)
	if err != nil {
		return Liked{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Liked{}, ErrNothingPlaying
	}
	var state struct {
		Item *struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Type    string   `json:"type"`
			Artists []artist `json:"artists"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return Liked{}, fmt.Errorf("spotify api: player state: %w", err)
	}
	if state.Item == nil || state.Item.ID == "" || (state.Item.Type != "" && state.Item.Type != "track") {
		return Liked{}, ErrNothingPlaying
	}
	if _, err := p.api.Put(ctx, "/me/tracks", url.Values{"ids": {state.Item.ID}}, nil); err != nil {
		return Liked{}, err
	}
	return Liked{Track: state.Item.Name, Artists: joinArtists(state.Item.Artists)}, nil
}
