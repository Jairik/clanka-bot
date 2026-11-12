package spotify_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	spotifytools "github.com/Jairik/clanka-bot/assistant/tools/spotify"
)

// webAPI is an httptest fake of the Spotify Web API. It never leaves the
// process and records each control call.
type webAPI struct {
	mu      sync.Mutex
	calls   []string
	devices string
	search  string
	player  string
	status  int // non-zero fails every control call
	server  *httptest.Server
}

func newWebAPI(t *testing.T) *webAPI {
	t.Helper()
	f := &webAPI{devices: `{"devices":[{"id":"pi","name":"Clanka","volume_percent":70}]}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+string(body))
		switch r.URL.Path {
		case "/me/player/devices":
			_, _ = w.Write([]byte(f.devices))
		case "/search":
			_, _ = w.Write([]byte(f.search))
		case "/me/player":
			if f.player == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte(f.player))
		default:
			if f.status != 0 {
				w.WriteHeader(f.status)
				_, _ = w.Write([]byte(`{"error":{"reason":"NO_ACTIVE_DEVICE"}}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *webAPI) calledWith(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// directAPI reaches the fake without OAuth; the real client's auth path is
// covered in internal/spotify.
type directAPI struct{ base string }

func (d directAPI) do(ctx context.Context, method, path string, q url.Values, body []byte) ([]byte, error) {
	target := d.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, method, target, strings.NewReader(string(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, &spotify.APIError{StatusCode: resp.StatusCode, Reason: "NO_ACTIVE_DEVICE"}
	}
	return raw, nil
}
func (d directAPI) Get(ctx context.Context, p string, q url.Values) ([]byte, error) {
	return d.do(ctx, "GET", p, q, nil)
}
func (d directAPI) Post(ctx context.Context, p string, q url.Values, b []byte) ([]byte, error) {
	return d.do(ctx, "POST", p, q, b)
}
func (d directAPI) Put(ctx context.Context, p string, q url.Values, b []byte) ([]byte, error) {
	return d.do(ctx, "PUT", p, q, b)
}

type rig struct {
	api   *webAPI
	focus *focus.Registry
	svc   *spotifytools.Service
	cap   int
}

func newRig(t *testing.T, cap int) *rig {
	t.Helper()
	api := newWebAPI(t)
	r := &rig{api: api, focus: focus.New(), cap: cap}
	player := spotify.NewPlayer(directAPI{api.server.URL}, "Clanka", func() int { return r.cap })
	r.svc = spotifytools.NewService(player, r.focus)
	return r
}

const radiohead = `{"artists":{"items":[{"name":"Radiohead","uri":"spotify:artist:rh"}]},"tracks":{"items":[]}}`

func TestPlayRadioheadClaimsMusicFocusAndSpeaks(t *testing.T) {
	r := newRig(t, 0)
	r.api.search = radiohead
	play := spotifytools.NewPlay(r.svc)
	out, err := play.Handler(t.Context(), spotifytools.PlayIn{Query: "Radiohead"})
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	if got := play.Speak(out); got != "Playing Radiohead." {
		t.Fatalf("Speak = %q", got)
	}
	if !r.api.calledWith(`PUT /me/player/play {"context_uri":"spotify:artist:rh"}`) {
		t.Fatalf("calls = %v", r.api.calls)
	}
	if !r.focus.Held(focus.Music) {
		t.Fatal("playing music did not claim Music Focus")
	}
}

func TestPauseNextResumeReachSpotifyAsFocusIntents(t *testing.T) {
	r := newRig(t, 0)
	r.api.search = radiohead
	if _, err := spotifytools.NewPlay(r.svc).Handler(t.Context(), spotifytools.PlayIn{Query: "Radiohead"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		intent focus.Intent
		call   string
		say    string
	}{
		{focus.Pause, "PUT /me/player/pause", "Paused."},
		{focus.Resume, "PUT /me/player/play", "Resuming."},
		{focus.Next, "POST /me/player/next", "Next track."},
	}
	for _, c := range cases {
		text, ok := r.focus.HandleIntent(t.Context(), c.intent)
		if !ok || text != c.say || !r.api.calledWith(c.call) {
			t.Fatalf("%s: text=%q ok=%v calls=%v", c.intent, text, ok, r.api.calls)
		}
	}
	// Paused music stays in Focus so a bare "resume" still reaches it.
	if !r.focus.Held(focus.Music) {
		t.Fatal("music lost Focus on pause")
	}
}

func TestStopPausesAndReleasesFocus(t *testing.T) {
	r := newRig(t, 0)
	r.api.search = radiohead
	_, _ = spotifytools.NewPlay(r.svc).Handler(t.Context(), spotifytools.PlayIn{Query: "Radiohead"})
	if _, ok := r.focus.HandleIntent(t.Context(), focus.Stop); !ok {
		t.Fatal("stop was not handled")
	}
	if r.focus.Held(focus.Music) {
		t.Fatal("stop did not release Music Focus")
	}
	if _, ok := r.focus.HandleIntent(t.Context(), focus.Next); ok {
		t.Fatal("next reached music after it released Focus")
	}
}

// ringHolder is a stand-in for a ringing alert at a higher priority.
type ringHolder struct{ got []focus.Intent }

func (*ringHolder) Intents() []focus.Intent { return []focus.Intent{focus.Stop, focus.Pause} }
func (h *ringHolder) HandleIntent(_ context.Context, i focus.Intent) (string, bool) {
	h.got = append(h.got, i)
	return "ring stopped", true
}

func TestARingHoldingFocusGetsTheIntentBeforeMusic(t *testing.T) {
	r := newRig(t, 0)
	r.api.search = radiohead
	_, _ = spotifytools.NewPlay(r.svc).Handler(t.Context(), spotifytools.PlayIn{Query: "Radiohead"})
	ring := &ringHolder{}
	claim := r.focus.Claim(focus.Ring, ring)
	text, _ := r.focus.HandleIntent(t.Context(), focus.Pause)
	if text != "ring stopped" || r.api.calledWith("/me/player/pause") {
		t.Fatalf("pause reached music while a Ring held Focus: %q", text)
	}
	// "next" is not a Ring intent, so it falls through to music.
	if _, ok := r.focus.HandleIntent(t.Context(), focus.Next); !ok {
		t.Fatal("next did not fall through to music")
	}
	claim.Release()
	if text, _ := r.focus.HandleIntent(t.Context(), focus.Pause); text != "Paused." {
		t.Fatalf("after the Ring, pause = %q", text)
	}
}

func TestLostDeviceReleasesFocusSoThePhraseFallsThrough(t *testing.T) {
	r := newRig(t, 0)
	r.api.search = radiohead
	_, _ = spotifytools.NewPlay(r.svc).Handler(t.Context(), spotifytools.PlayIn{Query: "Radiohead"})
	r.api.mu.Lock()
	r.api.devices = `{"devices":[]}`
	r.api.mu.Unlock()
	if _, ok := r.focus.HandleIntent(t.Context(), focus.Pause); ok {
		t.Fatal("pause was handled with no device")
	}
	if r.focus.Held(focus.Music) {
		t.Fatal("stale music Focus was kept")
	}
}

func TestPlayWithoutTheDeviceExplainsWhatToDo(t *testing.T) {
	r := newRig(t, 0)
	r.api.devices = `{"devices":[]}`
	_, err := spotifytools.NewPlay(r.svc).Handler(t.Context(), spotifytools.PlayIn{Query: "x"})
	te, ok := err.(*tool.ToolError)
	if !ok || !strings.Contains(te.Say, "pick it once") {
		t.Fatalf("err = %v", err)
	}
	if r.focus.Held(focus.Music) {
		t.Fatal("a failed play claimed Focus")
	}
}

func TestVolumeIsCappedOnlyWhenMaxVolumeIsSet(t *testing.T) {
	r := newRig(t, 0)
	vol := spotifytools.NewVolume(r.svc)
	ninety := 90
	out, err := vol.Handler(t.Context(), spotifytools.VolumeIn{Percent: &ninety})
	if err != nil || out.Percent != 90 || out.Capped {
		t.Fatalf("uncapped = %+v, %v", out, err)
	}
	r.cap = 55
	out, err = vol.Handler(t.Context(), spotifytools.VolumeIn{Percent: &ninety})
	if err != nil || out.Percent != 55 || !out.Capped || out.Cap != 55 {
		t.Fatalf("capped = %+v, %v", out, err)
	}
	if got := vol.Speak(out); got != "The music volume is capped at 55 percent." {
		t.Fatalf("Speak = %q", got)
	}
	if !r.api.calledWith("/me/player/volume") {
		t.Fatalf("calls = %v", r.api.calls)
	}
	over := 101
	if _, err := vol.Handler(t.Context(), spotifytools.VolumeIn{Percent: &over}); err == nil {
		t.Fatal("accepted 101")
	}
}

func TestNowPlayingSpeaksTrackPausedAndNothing(t *testing.T) {
	r := newRig(t, 0)
	np := spotifytools.NewNowPlaying(r.svc)
	out, err := np.Handler(t.Context(), spotifytools.NowPlayingIn{})
	if err != nil || np.Speak(out) != "Nothing is playing." {
		t.Fatalf("nothing: %+v %v", out, err)
	}
	r.api.mu.Lock()
	r.api.player = `{"is_playing":true,"device":{"name":"Clanka"},"item":{"name":"Creep","artists":[{"name":"Radiohead"}],"album":{"name":"Pablo Honey"}}}`
	r.api.mu.Unlock()
	out, _ = np.Handler(t.Context(), spotifytools.NowPlayingIn{})
	if got := np.Speak(out); got != "Creep by Radiohead is playing." {
		t.Fatalf("Speak = %q", got)
	}
	if info := np; info.LocalOnly != tool.LocalOnlyYes {
		t.Fatal("now_playing must stay on the Pi")
	}
}

func TestPlayExtractUnderstandsTheCommonPhrasings(t *testing.T) {
	play := spotifytools.NewPlay(newRig(t, 0).svc)
	cases := []struct {
		text string
		want spotifytools.PlayIn
	}{
		{"play Radiohead", spotifytools.PlayIn{Query: "Radiohead"}},
		{"Play some Radiohead.", spotifytools.PlayIn{Query: "Radiohead"}},
		{"play the album OK Computer", spotifytools.PlayIn{Query: "OK Computer", Kind: "album"}},
		{"play the song Creep by Radiohead", spotifytools.PlayIn{Query: "Creep Radiohead", Kind: "track"}},
		{"play my liked songs", spotifytools.PlayIn{Kind: "liked"}},
		{"play music by Taylor Swift", spotifytools.PlayIn{Query: "Taylor Swift", Kind: "artist"}},
		{"please play the playlist Focus Flow", spotifytools.PlayIn{Query: "Focus Flow", Kind: "playlist"}},
		{"resume the music", spotifytools.PlayIn{}},
		{"play", spotifytools.PlayIn{}},
	}
	for _, c := range cases {
		got, ok := play.Extract(c.text)
		if !ok || got.Query != c.want.Query || got.Kind != c.want.Kind {
			t.Errorf("Extract(%q) = %+v %v, want %+v", c.text, got, ok, c.want)
		}
	}
	if _, ok := play.Extract("what time is it"); ok {
		t.Error("Extract accepted a non-play request")
	}
}

func TestVolumeExtractNeedsTheWordMusic(t *testing.T) {
	vol := spotifytools.NewVolume(newRig(t, 0).svc)
	if got, ok := vol.Extract("set the music volume to 40 percent"); !ok || got.Percent == nil || *got.Percent != 40 {
		t.Fatalf("percent = %+v %v", got, ok)
	}
	if got, ok := vol.Extract("turn the music down"); !ok || got.Delta >= 0 {
		t.Fatalf("down = %+v %v", got, ok)
	}
	if got, ok := vol.Extract("make the music louder"); !ok || got.Delta <= 0 {
		t.Fatalf("up = %+v %v", got, ok)
	}
	if _, ok := vol.Extract("turn the volume up"); ok {
		t.Fatal("a plain volume request must stay with audio.volume_set")
	}
}
