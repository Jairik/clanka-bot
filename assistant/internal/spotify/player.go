package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MaxVolumeKey is the live setting that caps Spotify's own volume. Zero is
// "no cap" and is the shipped default: the #70 hardware spike chooses a value.
const MaxVolumeKey = "music.max_volume"

// Kinds a play request can ask for. Empty means "search everything".
const (
	KindTrack    = "track"
	KindArtist   = "artist"
	KindAlbum    = "album"
	KindPlaylist = "playlist"
	KindLiked    = "liked"
)

var (
	// ErrDeviceNotFound means the receiver device is not in the account's
	// Connect device list, so it is not paired or not running.
	ErrDeviceNotFound = errors.New("spotify: the Pi's Connect device is not available")
	// ErrNothingFound means a search matched nothing playable.
	ErrNothingFound = errors.New("spotify: nothing matched")
	// ErrNothingPlaying means the account has no playback session.
	ErrNothingPlaying = errors.New("spotify: nothing is playing")
)

// API is the Web API seam Player drives. *Client satisfies it; keeping it an
// interface lets a test aim the Player at an httptest fake.
type API interface {
	Get(ctx context.Context, path string, query url.Values) ([]byte, error)
	Post(ctx context.Context, path string, query url.Values, body []byte) ([]byte, error)
	Put(ctx context.Context, path string, query url.Values, body []byte) ([]byte, error)
}

// PlayRequest is one play call. An empty Query with no Kind resumes. Volume is
// an optional level to set with the start, still subject to the cap.
type PlayRequest struct {
	Query  string
	Kind   string
	Volume *int
}

// Played describes what a play call started, for the spoken reply.
type Played struct {
	Name   string
	Artist string
	Kind   string
	// Resumed is true when there was no query and playback was resumed.
	Resumed bool
	// Volume is the level set by this call, 0 when none was.
	Volume int
	// Capped is true when music.max_volume lowered Volume or the device level.
	Capped bool
}

// NowPlaying is the current playback state.
type NowPlaying struct {
	Track   string
	Artists string
	Album   string
	Playing bool
	Device  string
	Volume  int
}

// Player is the playback control seam over the Web API. It always targets the
// one receiver device by name so a phone or desktop elsewhere is never
// driven by accident. The playlist and library methods (#73) are in library.go.
type Player struct {
	api        API
	deviceName string
	maxVolume  func() int
}

// NewPlayer builds a Player. maxVolume is read per call so a Dashboard change
// applies live; nil or a result <= 0 means no cap.
func NewPlayer(api API, deviceName string, maxVolume func() int) *Player {
	if strings.TrimSpace(deviceName) == "" {
		deviceName = DefaultDeviceName
	}
	return &Player{api: api, deviceName: deviceName, maxVolume: maxVolume}
}

// Cap is the current volume ceiling, 0 when unset.
func (p *Player) Cap() int {
	if p == nil || p.maxVolume == nil {
		return 0
	}
	if limit := p.maxVolume(); limit > 0 && limit < 100 {
		return limit
	}
	return 0
}

// ClampVolume applies the cap and reports whether it lowered the value.
func (p *Player) ClampVolume(percent int) (int, bool) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if limit := p.Cap(); limit > 0 && percent > limit {
		return limit, true
	}
	return percent, false
}

// device is one entry of GET /me/player/devices.
type device struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Volume *int   `json:"volume_percent"`
}

// findDevice looks the receiver device up by name on every call. A cached ID
// would go stale whenever the receiver restarts, and the lookup is one cheap GET.
func (p *Player) findDevice(ctx context.Context) (device, error) {
	raw, err := p.api.Get(ctx, "/me/player/devices", nil)
	if err != nil {
		return device{}, err
	}
	var list struct {
		Devices []device `json:"devices"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return device{}, fmt.Errorf("spotify api: devices: %w", err)
	}
	for _, d := range list.Devices {
		if strings.EqualFold(strings.TrimSpace(d.Name), strings.TrimSpace(p.deviceName)) && d.ID != "" {
			return d, nil
		}
	}
	return device{}, ErrDeviceNotFound
}

// deviceQuery targets the Pi device on a control call.
func deviceQuery(d device) url.Values {
	return url.Values{"device_id": {d.ID}}
}

// Pause pauses the Pi's playback.
func (p *Player) Pause(ctx context.Context) error {
	d, err := p.findDevice(ctx)
	if err != nil {
		return err
	}
	_, err = p.api.Put(ctx, "/me/player/pause", deviceQuery(d), nil)
	return ignoreRestriction(err)
}

// Resume continues the Pi's paused playback.
func (p *Player) Resume(ctx context.Context) error {
	d, err := p.findDevice(ctx)
	if err != nil {
		return err
	}
	// The cap is a ceiling on the music, so a resume cannot play above it.
	if err := p.applyStartVolume(ctx, d, nil, &Played{}); err != nil {
		return err
	}
	_, err = p.api.Put(ctx, "/me/player/play", deviceQuery(d), nil)
	return ignoreRestriction(err)
}

// ignoreRestriction treats Spotify's 403 "Restriction violated" (pause while
// paused, play while playing) as the state the user asked for. Only the
// documented ALREADY_PAUSED / ALREADY_PLAYING reasons or the "Restriction
// violated" message count; any other 403 (Premium required, a missing OAuth
// scope) is a real failure and stays one.
func ignoreRestriction(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		return err
	}
	switch apiErr.Reason {
	case "ALREADY_PAUSED", "ALREADY_PLAYING":
		return nil
	case "PREMIUM_REQUIRED":
		return err
	}
	if apiErr.Restricted {
		return nil
	}
	return err
}

// Next skips to the next track on the Pi.
func (p *Player) Next(ctx context.Context) error {
	d, err := p.findDevice(ctx)
	if err != nil {
		return err
	}
	_, err = p.api.Post(ctx, "/me/player/next", deviceQuery(d), nil)
	return err
}

// SetVolume sets Spotify's volume on the Pi device, honouring the cap. It
// returns the applied level and whether the cap changed it.
func (p *Player) SetVolume(ctx context.Context, percent int) (int, bool, error) {
	d, err := p.findDevice(ctx)
	if err != nil {
		return 0, false, err
	}
	applied, capped := p.ClampVolume(percent)
	return applied, capped, p.putVolume(ctx, d, applied)
}

// AdjustVolume moves the volume by delta from the device's current level.
func (p *Player) AdjustVolume(ctx context.Context, delta int) (int, bool, error) {
	d, err := p.findDevice(ctx)
	if err != nil {
		return 0, false, err
	}
	current := 0
	if d.Volume != nil {
		current = *d.Volume
	}
	applied, capped := p.ClampVolume(current + delta)
	return applied, capped, p.putVolume(ctx, d, applied)
}

func (p *Player) putVolume(ctx context.Context, d device, percent int) error {
	query := deviceQuery(d)
	query.Set("volume_percent", strconv.Itoa(percent))
	_, err := p.api.Put(ctx, "/me/player/volume", query, nil)
	return err
}

// NowPlaying reads the account's playback state. ErrNothingPlaying means no
// session or no current item.
func (p *Player) NowPlaying(ctx context.Context) (NowPlaying, error) {
	raw, err := p.api.Get(ctx, "/me/player", nil)
	if err != nil {
		return NowPlaying{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return NowPlaying{}, ErrNothingPlaying
	}
	var state struct {
		Playing bool `json:"is_playing"`
		Device  struct {
			Name   string `json:"name"`
			Volume *int   `json:"volume_percent"`
		} `json:"device"`
		Item *struct {
			Name    string   `json:"name"`
			Artists []artist `json:"artists"`
			Album   struct {
				Name string `json:"name"`
			} `json:"album"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return NowPlaying{}, fmt.Errorf("spotify api: player state: %w", err)
	}
	if state.Item == nil || state.Item.Name == "" {
		return NowPlaying{}, ErrNothingPlaying
	}
	out := NowPlaying{
		Track:   state.Item.Name,
		Artists: joinArtists(state.Item.Artists),
		Album:   state.Item.Album.Name,
		Playing: state.Playing,
		Device:  state.Device.Name,
	}
	if state.Device.Volume != nil {
		out.Volume = *state.Device.Volume
	}
	return out, nil
}

// artist is the name-only artist object Spotify nests in tracks and albums.
type artist struct {
	Name string `json:"name"`
}

func joinArtists(artists []artist) string {
	names := make([]string, 0, len(artists))
	for _, a := range artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// Play resumes with no query, or searches and starts the best match on the Pi.
func (p *Player) Play(ctx context.Context, req PlayRequest) (Played, error) {
	d, err := p.findDevice(ctx)
	if err != nil {
		return Played{}, err
	}
	out := Played{}
	query := strings.TrimSpace(req.Query)
	kind := strings.ToLower(strings.TrimSpace(req.Kind))

	var body []byte
	switch {
	case kind == KindLiked:
		uris, err := p.likedTracks(ctx)
		if err != nil {
			return Played{}, err
		}
		body, _ = json.Marshal(map[string]any{"uris": uris})
		out.Name, out.Kind = "your liked songs", KindLiked
	case query == "":
		out.Resumed = true
	default:
		hit, err := p.search(ctx, query, kind)
		if err != nil {
			return Played{}, err
		}
		if hit.Kind == KindTrack {
			body, _ = json.Marshal(map[string]any{"uris": []string{hit.URI}})
		} else {
			body, _ = json.Marshal(map[string]any{"context_uri": hit.URI})
		}
		out.Name, out.Artist, out.Kind = hit.Name, hit.Artist, hit.Kind
	}

	if err := p.applyStartVolume(ctx, d, req.Volume, &out); err != nil {
		return Played{}, err
	}

	if _, err := p.api.Put(ctx, "/me/player/play", deviceQuery(d), body); err != nil {
		return Played{}, err
	}
	return out, nil
}

// applyStartVolume sets the volume before the first note, so a cap lands
// before playback. An explicit level is clamped; otherwise a device that is
// already louder than the cap is lowered to it, because the cap is a ceiling on
// the music, not only on the voice command. It records the result in out.
func (p *Player) applyStartVolume(ctx context.Context, d device, requested *int, out *Played) error {
	switch {
	case requested != nil:
		out.Volume, out.Capped = p.ClampVolume(*requested)
		return p.putVolume(ctx, d, out.Volume)
	case p.Cap() > 0 && d.Volume != nil && *d.Volume > p.Cap():
		out.Volume, out.Capped = p.Cap(), true
		return p.putVolume(ctx, d, out.Volume)
	}
	return nil
}

// hit is one search result chosen to play.
type hit struct {
	Kind, Name, Artist, URI string
}

// likedTracks returns the newest saved tracks. A play call accepts at most 50
// URIs, so this is the most recent 50 likes, not the whole library.
func (p *Player) likedTracks(ctx context.Context) ([]string, error) {
	raw, err := p.api.Get(ctx, "/me/tracks", url.Values{"limit": {"50"}})
	if err != nil {
		return nil, err
	}
	var page struct {
		Items []struct {
			Track struct {
				URI string `json:"uri"`
			} `json:"track"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("spotify api: liked songs: %w", err)
	}
	var uris []string
	for _, item := range page.Items {
		if item.Track.URI != "" {
			uris = append(uris, item.Track.URI)
		}
	}
	if len(uris) == 0 {
		return nil, ErrNothingFound
	}
	return uris, nil
}

// search finds the best playable match. With a kind it takes that type's top
// result. Without one it prefers an exact name match (artist, album, playlist,
// then track) and otherwise Spotify's top track, so "play Radiohead" starts the
// artist while "play Creep" starts the song.
func (p *Player) search(ctx context.Context, query, kind string) (hit, error) {
	types := []string{KindTrack, KindArtist, KindAlbum, KindPlaylist}
	if kind != "" {
		switch kind {
		case KindTrack, KindArtist, KindAlbum, KindPlaylist:
			types = []string{kind}
		default:
			return hit{}, fmt.Errorf("spotify: unknown kind %q", kind)
		}
	}
	raw, err := p.api.Get(ctx, "/search", url.Values{
		"q":     {query},
		"type":  {strings.Join(types, ",")},
		"limit": {"3"},
	})
	if err != nil {
		return hit{}, err
	}
	var found struct {
		Tracks struct {
			Items []struct {
				Name, URI string
				Artists   []artist
			}
		}
		Artists struct{ Items []struct{ Name, URI string } }
		Albums  struct {
			Items []struct {
				Name, URI string
				Artists   []artist
			}
		}
		Playlists struct{ Items []*struct{ Name, URI string } }
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		return hit{}, fmt.Errorf("spotify api: search: %w", err)
	}
	var candidates []hit
	for _, a := range found.Artists.Items {
		candidates = append(candidates, hit{KindArtist, a.Name, "", a.URI})
	}
	for _, a := range found.Albums.Items {
		candidates = append(candidates, hit{KindAlbum, a.Name, joinArtists(a.Artists), a.URI})
	}
	for _, pl := range found.Playlists.Items {
		if pl != nil {
			candidates = append(candidates, hit{KindPlaylist, pl.Name, "", pl.URI})
		}
	}
	for _, t := range found.Tracks.Items {
		candidates = append(candidates, hit{KindTrack, t.Name, joinArtists(t.Artists), t.URI})
	}
	// With a kind requested, the first candidate is that type's top result.
	// Otherwise an exact name match wins in the candidate order above.
	if kind == "" {
		for _, c := range candidates {
			if c.URI != "" && strings.EqualFold(strings.TrimSpace(c.Name), query) {
				return c, nil
			}
		}
		for _, t := range found.Tracks.Items {
			if t.URI != "" {
				return hit{KindTrack, t.Name, joinArtists(t.Artists), t.URI}, nil
			}
		}
	}
	for _, c := range candidates {
		if c.URI != "" {
			return c, nil
		}
	}
	return hit{}, ErrNothingFound
}

// APIStatus extracts the HTTP status and machine reason from an error, or
// zero when it is not a Web API response.
func APIStatus(err error) (status int, reason string) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, apiErr.Reason
	}
	return 0, ""
}

var _ API = (*Client)(nil)
