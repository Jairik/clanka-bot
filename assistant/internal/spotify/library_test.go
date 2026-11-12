package spotify_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

func TestPlayPlaylistTargetsThePiAndSetsShuffleFirst(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	out, err := p.PlayPlaylist(t.Context(), spotify.PlaylistPlay{Name: "Running", URI: "spotify:playlist:run", Shuffle: true})
	if err != nil {
		t.Fatalf("PlayPlaylist: %v", err)
	}
	if out.Name != "Running" || out.Kind != spotify.KindPlaylist {
		t.Fatalf("played = %+v", out)
	}
	calls := api.all()
	shuffle, play := calls[len(calls)-2], calls[len(calls)-1]
	if !strings.HasPrefix(shuffle, "PUT /me/player/shuffle?") || !strings.Contains(shuffle, "state=true") || !strings.Contains(shuffle, "device_id=pi-id") {
		t.Fatalf("shuffle call = %q", shuffle)
	}
	if want := `PUT /me/player/play?device_id=pi-id {"context_uri":"spotify:playlist:run"}`; play != want {
		t.Fatalf("play call = %q, want %q", play, want)
	}
}

func TestPlayPlaylistWithoutShuffleLeavesShuffleAlone(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	if _, err := p.PlayPlaylist(t.Context(), spotify.PlaylistPlay{Name: "Running", URI: "spotify:playlist:run"}); err != nil {
		t.Fatalf("PlayPlaylist: %v", err)
	}
	for _, call := range api.all() {
		if strings.Contains(call, "/shuffle") {
			t.Fatalf("unrequested shuffle call: %q", call)
		}
	}
}

func TestPlayPlaylistHonoursTheMaxVolumeCap(t *testing.T) {
	p, api := newPlayerFixture(t, 50)
	out, err := p.PlayPlaylist(t.Context(), spotify.PlaylistPlay{Name: "Running", URI: "spotify:playlist:run"})
	if err != nil || out.Volume != 50 || !out.Capped {
		t.Fatalf("played = %+v, %v", out, err)
	}
	calls := api.all()
	if got := calls[len(calls)-2]; !strings.Contains(got, "volume_percent=50") {
		t.Fatalf("volume call before play = %q", got)
	}
}

func TestPlayPlaylistNeedsAURI(t *testing.T) {
	p, _ := newPlayerFixture(t, 0)
	if _, err := p.PlayPlaylist(t.Context(), spotify.PlaylistPlay{Name: "x"}); !errors.Is(err, spotify.ErrNothingFound) {
		t.Fatalf("err = %v, want ErrNothingFound", err)
	}
}

func TestShuffleAndRepeatTargetThePi(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	if err := p.SetShuffle(t.Context(), false); err != nil {
		t.Fatalf("SetShuffle: %v", err)
	}
	if got := api.last(); !strings.Contains(got, "PUT /me/player/shuffle?") || !strings.Contains(got, "state=false") || !strings.Contains(got, "device_id=pi-id") {
		t.Fatalf("shuffle call = %q", got)
	}
	for _, mode := range []string{spotify.RepeatOff, spotify.RepeatTrack, spotify.RepeatContext} {
		if err := p.SetRepeat(t.Context(), mode); err != nil {
			t.Fatalf("SetRepeat %s: %v", mode, err)
		}
		if got := api.last(); !strings.Contains(got, "/me/player/repeat?") || !strings.Contains(got, "state="+mode) {
			t.Fatalf("repeat call = %q", got)
		}
	}
	before := len(api.all())
	if err := p.SetRepeat(t.Context(), "forever"); err == nil {
		t.Fatal("unknown repeat mode was sent")
	}
	if len(api.all()) != before {
		t.Fatal("an invalid repeat mode reached the Web API")
	}
}

func TestPreviousTargetsThePi(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	if err := p.Previous(t.Context()); err != nil {
		t.Fatalf("Previous: %v", err)
	}
	if got, want := api.last(), "POST /me/player/previous?device_id=pi-id "; got != want {
		t.Fatalf("call = %q, want %q", got, want)
	}
}

func TestQueueAddSearchesTracksAndQueuesOnThePi(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.search = `{"tracks":{"items":[{"name":"Creep","uri":"spotify:track:creep","artists":[{"name":"Radiohead"}]}]}}`
	got, err := p.QueueAdd(t.Context(), "Creep Radiohead")
	if err != nil || got.Name != "Creep" || got.Artist != "Radiohead" {
		t.Fatalf("QueueAdd = %+v, %v", got, err)
	}
	calls := api.all()
	if s := calls[1]; !strings.Contains(s, "type=track") {
		t.Fatalf("search call = %q, want tracks only", s)
	}
	last := api.last()
	if !strings.HasPrefix(last, "POST /me/player/queue?") || !strings.Contains(last, "uri=spotify%3Atrack%3Acreep") || !strings.Contains(last, "device_id=pi-id") {
		t.Fatalf("queue call = %q", last)
	}
}

func TestQueueAddWithNothingFoundSaysSo(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.search = `{"tracks":{"items":[]}}`
	if _, err := p.QueueAdd(t.Context(), "zzzz"); !errors.Is(err, spotify.ErrNothingFound) {
		t.Fatalf("err = %v, want ErrNothingFound", err)
	}
	if _, err := p.QueueAdd(t.Context(), "  "); !errors.Is(err, spotify.ErrNothingFound) {
		t.Fatalf("blank query err = %v, want ErrNothingFound", err)
	}
}

func TestLikeSavesTheCurrentTrack(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.player = `{"is_playing":true,"device":{"name":"Clanka"},
		"item":{"id":"abc123","name":"Creep","artists":[{"name":"Radiohead"}],"album":{"name":"Pablo Honey"}}}`
	got, err := p.LikeCurrent(t.Context())
	if err != nil || got.Track != "Creep" || got.Artists != "Radiohead" {
		t.Fatalf("LikeCurrent = %+v, %v", got, err)
	}
	if last := api.last(); !strings.HasPrefix(last, "PUT /me/tracks?") || !strings.Contains(last, "ids=abc123") {
		t.Fatalf("like call = %q", last)
	}
}

func TestLikeWithNothingPlayingSavesNothing(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	if _, err := p.LikeCurrent(t.Context()); !errors.Is(err, spotify.ErrNothingPlaying) {
		t.Fatalf("err = %v, want ErrNothingPlaying", err)
	}
	for _, call := range api.all() {
		if strings.HasPrefix(call, "PUT /me/tracks") {
			t.Fatalf("saved a track with nothing playing: %q", call)
		}
	}
}

func TestMissingPiDeviceBlocksThePlaylistControls(t *testing.T) {
	p, api := newPlayerFixture(t, 0)
	api.devices = `{"devices":[{"id":"phone-id","name":"Phone"}]}`
	if err := p.SetShuffle(t.Context(), true); !errors.Is(err, spotify.ErrDeviceNotFound) {
		t.Fatalf("shuffle err = %v", err)
	}
	if err := p.Previous(t.Context()); !errors.Is(err, spotify.ErrDeviceNotFound) {
		t.Fatalf("previous err = %v", err)
	}
}
