package spotify_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// fakeWebAPI is an httptest Web API: a device list, a search result and a
// recorder of every control call. It never contacts Spotify.
type fakeWebAPI struct {
	mu       sync.Mutex
	calls    []string // "METHOD path?query body"
	devices  string
	search   string
	player   string
	liked    string
	failWith int
}

func (f *fakeWebAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		f.calls = append(f.calls, r.Method+" "+path+"?"+r.URL.RawQuery+" "+string(body))
		if f.failWith != 0 && path != "/me/player/devices" {
			w.WriteHeader(f.failWith)
			_, _ = w.Write([]byte(`{"error":{"status":404,"reason":"NO_ACTIVE_DEVICE"}}`))
			return
		}
		switch path {
		case "/me/player/devices":
			_, _ = w.Write([]byte(f.devices))
		case "/search":
			_, _ = w.Write([]byte(f.search))
		case "/me/tracks":
			_, _ = w.Write([]byte(f.liked))
		case "/me/player":
			if f.player == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte(f.player))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func (f *fakeWebAPI) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeWebAPI) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

const twoDevices = `{"devices":[{"id":"phone-id","name":"Phone","volume_percent":10},{"id":"pi-id","name":"clanka","volume_percent":70}]}`

func newPlayerFixture(t *testing.T, cap int) (*spotify.Player, *fakeWebAPI) {
	t.Helper()
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	api := &fakeWebAPI{devices: twoDevices}
	f.fake.mu.Lock()
	f.fake.api = api.handler(t)
	f.fake.mu.Unlock()
	return spotify.NewPlayer(f.auth.Client(), "Clanka", func() int { return cap }), api
}

func TestPlayRadioheadStartsTheArtistOnThePiDevice(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.search = `{"tracks":{"items":[{"name":"Karma Police","uri":"spotify:track:kp","artists":[{"name":"Radiohead"}]}]},
		"artists":{"items":[{"name":"Radiohead","uri":"spotify:artist:rh"}]},
		"albums":{"items":[]},"playlists":{"items":[null]}}`
	out, err := p.Play(t.Context(), spotify.PlayRequest{Query: "Radiohead"})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if out.Kind != spotify.KindArtist || out.Name != "Radiohead" {
		t.Fatalf("played = %+v", out)
	}
	want := `PUT /me/player/play?device_id=pi-id {"context_uri":"spotify:artist:rh"}`
	if api.last() != want {
		t.Fatalf("last call = %q, want %q", api.last(), want)
	}
}

func TestPlayPrefersTheTopTrackWhenNoNameMatches(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.search = `{"tracks":{"items":[{"name":"Creep","uri":"spotify:track:creep","artists":[{"name":"Radiohead"}]}]},
		"artists":{"items":[{"name":"Creeper","uri":"spotify:artist:x"}]}}`
	out, err := p.Play(t.Context(), spotify.PlayRequest{Query: "creep song"})
	if err != nil || out.Kind != spotify.KindTrack || out.Artist != "Radiohead" {
		t.Fatalf("played = %+v, %v", out, err)
	}
	if !strings.Contains(api.last(), `{"uris":["spotify:track:creep"]}`) {
		t.Fatalf("last call = %q", api.last())
	}
}

func TestPlayWithAKindSearchesOnlyThatType(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.search = `{"albums":{"items":[{"name":"OK Computer","uri":"spotify:album:ok","artists":[{"name":"Radiohead"}]}]}}`
	out, err := p.Play(t.Context(), spotify.PlayRequest{Query: "OK Computer", Kind: spotify.KindAlbum})
	if err != nil || out.Kind != spotify.KindAlbum {
		t.Fatalf("played = %+v, %v", out, err)
	}
	if got := api.all()[1]; !strings.Contains(got, "type=album") {
		t.Fatalf("search call = %q, want album only", got)
	}
}

func TestPlayLikedSongsUsesTheSavedTracks(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.liked = `{"items":[{"track":{"uri":"spotify:track:a"}},{"track":{"uri":"spotify:track:b"}}]}`
	if _, err := p.Play(t.Context(), spotify.PlayRequest{Kind: spotify.KindLiked}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if want := `{"uris":["spotify:track:a","spotify:track:b"]}`; !strings.HasSuffix(api.last(), want) {
		t.Fatalf("last call = %q", api.last())
	}
}

func TestPlayWithNothingFoundSaysSo(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.search = `{"tracks":{"items":[]}}`
	if _, err := p.Play(t.Context(), spotify.PlayRequest{Query: "zzzz"}); !errors.Is(err, spotify.ErrNothingFound) {
		t.Fatalf("err = %v, want ErrNothingFound", err)
	}
}

func TestResumePauseNextTargetOnlyThePiDevice(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	if _, err := p.Play(t.Context(), spotify.PlayRequest{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, want := api.last(), "PUT /me/player/play?device_id=pi-id "; got != want {
		t.Fatalf("resume call = %q, want %q", got, want)
	}
	if err := p.Pause(t.Context()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, want := api.last(), "PUT /me/player/pause?device_id=pi-id "; got != want {
		t.Fatalf("pause call = %q", got)
	}
	if err := p.Next(t.Context()); err != nil {
		t.Fatalf("next: %v", err)
	}
	if got, want := api.last(), "POST /me/player/next?device_id=pi-id "; got != want {
		t.Fatalf("next call = %q", got)
	}
}

func TestMissingPiDeviceIsAClearError(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.devices = `{"devices":[{"id":"phone-id","name":"Phone"}]}`
	if err := p.Pause(t.Context()); !errors.Is(err, spotify.ErrDeviceNotFound) {
		t.Fatalf("err = %v, want ErrDeviceNotFound", err)
	}
	for _, call := range api.all() {
		if strings.Contains(call, "/pause") {
			t.Fatalf("pause reached another device: %q", call)
		}
	}
}

func TestVolumeWithNoCapIsNotLimited(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	applied, capped, err := p.SetVolume(t.Context(), 95)
	if err != nil || applied != 95 || capped {
		t.Fatalf("SetVolume = %d %v %v", applied, capped, err)
	}
	if got := api.last(); !strings.Contains(got, "volume_percent=95") || !strings.Contains(got, "device_id=pi-id") {
		t.Fatalf("call = %q", got)
	}
}

func TestMaxVolumeCapsSetAndAdjust(t *testing.T) {
	p, api := newPlayerFixture(t, 60)
	applied, capped, err := p.SetVolume(t.Context(), 95)
	if err != nil || applied != 60 || !capped {
		t.Fatalf("SetVolume = %d %v %v, want 60 capped", applied, capped, err)
	}
	if !strings.Contains(api.last(), "volume_percent=60") {
		t.Fatalf("call = %q", api.last())
	}
	// The device is at 70; a +10 request lands on the cap, not 80.
	applied, capped, err = p.AdjustVolume(t.Context(), 10)
	if err != nil || applied != 60 || !capped {
		t.Fatalf("AdjustVolume = %d %v %v", applied, capped, err)
	}
	// Lowering is never blocked by the cap.
	if applied, capped, _ = p.AdjustVolume(t.Context(), -30); applied != 40 || capped {
		t.Fatalf("lower = %d %v, want 40 uncapped", applied, capped)
	}
}

func TestPlayEnforcesTheCapOnItsOwnVolumeAndOnALoudDevice(t *testing.T) {
	p, api := newPlayerFixture(t, 50)
	vol := 90
	out, err := p.Play(t.Context(), spotify.PlayRequest{Volume: &vol})
	if err != nil || out.Volume != 50 || !out.Capped {
		t.Fatalf("play volume = %+v, %v", out, err)
	}
	// Without an explicit level, the Pi device at 70 is lowered to the cap.
	out, err = p.Play(t.Context(), spotify.PlayRequest{})
	if err != nil || out.Volume != 50 || !out.Capped {
		t.Fatalf("play loud device = %+v, %v", out, err)
	}
	calls := api.all()
	if got := calls[len(calls)-2]; !strings.Contains(got, "volume_percent=50") {
		t.Fatalf("volume call before play = %q", got)
	}
}

func TestNowPlayingReportsTrackAndNothing(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	if _, err := p.NowPlaying(t.Context()); !errors.Is(err, spotify.ErrNothingPlaying) {
		t.Fatalf("err = %v, want ErrNothingPlaying", err)
	}
	api.player = `{"is_playing":true,"device":{"name":"Clanka","volume_percent":40},
		"item":{"name":"Creep","artists":[{"name":"Radiohead"}],"album":{"name":"Pablo Honey"}}}`
	np, err := p.NowPlaying(t.Context())
	if err != nil || np.Track != "Creep" || np.Artists != "Radiohead" || !np.Playing || np.Volume != 40 {
		t.Fatalf("NowPlaying = %+v, %v", np, err)
	}
}

func TestAPIStatusExposesTheMachineReason(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.failWith = http.StatusNotFound
	err := p.Pause(t.Context())
	if status, reason := spotify.APIStatus(err); status != 404 || reason != "NO_ACTIVE_DEVICE" {
		t.Fatalf("APIStatus = %d %q (%v)", status, reason, err)
	}
}
