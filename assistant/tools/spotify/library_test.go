package spotify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	spotifytools "github.com/Jairik/clanka-bot/assistant/tools/spotify"
)

const userPlaylists = `{"items":[
	{"name":"Running","uri":"spotify:playlist:run"},
	{"name":"Chill Vibes","uri":"spotify:playlist:chill"},
	{"name":"Focus Flow","uri":"spotify:playlist:focus"}],"next":null}`

// keywordEmbedder gives each keyword its own axis, so "jog" is near "Running"
// and nothing else is near anything, like the name matcher's own tests.
type keywordEmbedder struct{ calls int }

func (e *keywordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 4)
		lower := strings.ToLower(text)
		switch {
		case strings.Contains(lower, "run") || strings.Contains(lower, "jog"):
			v[0] = 1
		case strings.Contains(lower, "chill"):
			v[1] = 1
		case strings.Contains(lower, "focus"):
			v[2] = 1
		default:
			v[3] = 1
		}
		out[i] = v
	}
	return out, nil
}

type libRig struct {
	*rig
	embed *keywordEmbedder
	cache *spotify.PlaylistCache
}

func newLibRig(t *testing.T, cap int) *libRig {
	t.Helper()
	base := newRig(t, cap)
	base.api.lists = userPlaylists
	player := spotify.NewPlayer(directAPI{base.api.server.URL}, "Clanka", func() int { return base.cap })
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: directAPI{base.api.server.URL}})
	emb := &keywordEmbedder{}
	base.svc = spotifytools.NewService(player, base.focus).
		WithLibrary(player, cache, namematch.New(emb, namematch.Config{}))
	return &libRig{rig: base, embed: emb, cache: cache}
}

func TestPlayRunningPlaylistOnShuffleResolvesThroughTheCacheAndMatcher(t *testing.T) {
	r := newLibRig(t, 0)
	if err := r.cache.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	tl := spotifytools.NewPlayPlaylist(r.svc)
	in, ok := tl.Extract("play my running playlist on shuffle")
	if !ok || in.Name != "running" || !in.Shuffle {
		t.Fatalf("Extract = %+v %v", in, ok)
	}
	out, err := tl.Handler(t.Context(), in)
	if err != nil {
		t.Fatalf("play_playlist: %v", err)
	}
	if got := tl.Speak(out); got != "Playing your Running playlist on shuffle." {
		t.Fatalf("Speak = %q", got)
	}
	if !r.api.calledWith("PUT /me/player/shuffle") || !r.api.calledWith(`{"context_uri":"spotify:playlist:run"}`) {
		t.Fatalf("calls = %v", r.api.calls)
	}
	if !r.focus.Held(focus.Music) {
		t.Fatal("play_playlist did not claim Music Focus")
	}
	// "running" normalised-matches "Running" without an embed call.
	if r.embed.calls != 0 {
		t.Fatalf("embed calls = %d, want an exact match", r.embed.calls)
	}
}

func TestPlayPlaylistUsesSimilarityForANearName(t *testing.T) {
	r := newLibRig(t, 0)
	_ = r.cache.Refresh(t.Context())
	out, err := spotifytools.NewPlayPlaylist(r.svc).Handler(t.Context(), spotifytools.PlayPlaylistIn{Name: "my jogging mix"})
	if err != nil || out.Name != "Running" {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if r.embed.calls != 1 {
		t.Fatalf("embed calls = %d, want 1", r.embed.calls)
	}
}

func TestPlayPlaylistNeverForcesAGuess(t *testing.T) {
	r := newLibRig(t, 0)
	_ = r.cache.Refresh(t.Context())
	_, err := spotifytools.NewPlayPlaylist(r.svc).Handler(t.Context(), spotifytools.PlayPlaylistIn{Name: "polka marathon"})
	te, ok := err.(*tool.ToolError)
	if !ok || !strings.Contains(te.Say, "polka marathon") {
		t.Fatalf("err = %v, want a not-found ToolError naming the request", err)
	}
	if r.api.calledWith("/me/player/play") || r.focus.Held(focus.Music) {
		t.Fatal("a non-match started playback or claimed Focus")
	}
}

func TestPlayPlaylistLoadsTheCacheOnDemandWhenStartupHasNotFinished(t *testing.T) {
	r := newLibRig(t, 0)
	if r.cache.Loaded() {
		t.Fatal("cache loaded before use")
	}
	if _, err := spotifytools.NewPlayPlaylist(r.svc).Handler(t.Context(), spotifytools.PlayPlaylistIn{Name: "chill vibes"}); err != nil {
		t.Fatalf("play_playlist: %v", err)
	}
	if !r.cache.Loaded() {
		t.Fatal("cache was not loaded on demand")
	}
}

func TestPlayPlaylistWithAnUnreachableAPISpeaksTheSameErrorsAsPlay(t *testing.T) {
	r := newLibRig(t, 0)
	r.api.lists = ""
	// /me/playlists answers 200 "" here; an empty body is a malformed page.
	_, err := spotifytools.NewPlayPlaylist(r.svc).Handler(t.Context(), spotifytools.PlayPlaylistIn{Name: "running"})
	te, ok := err.(*tool.ToolError)
	if !ok || te.Say == "" || strings.Contains(te.Say, "spotify:") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlayPlaylistHonoursTheMaxVolumeCap(t *testing.T) {
	r := newLibRig(t, 40)
	out, err := spotifytools.NewPlayPlaylist(r.svc).Handler(t.Context(), spotifytools.PlayPlaylistIn{Name: "running"})
	if err != nil || !out.Capped || out.Volume != 40 {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if !r.api.calledQuery("volume_percent=40") {
		t.Fatalf("calls = %v", r.api.calls)
	}
	if got := spotifytools.NewPlayPlaylist(r.svc).Speak(out); !strings.Contains(got, "capped at 40 percent") {
		t.Fatalf("Speak = %q", got)
	}
}

func TestPlayPlaylistExtractUnderstandsTheCommonPhrasings(t *testing.T) {
	tl := spotifytools.NewPlayPlaylist(newLibRig(t, 0).svc)
	cases := []struct {
		text    string
		name    string
		shuffle bool
	}{
		{"play my running playlist", "running", false},
		{"Play my Chill Vibes playlist.", "Chill Vibes", false},
		{"play my running playlist on shuffle", "running", true},
		{"shuffle my running playlist", "running", true},
		{"play my workout playlist shuffled", "workout", true},
		{"play the playlist Focus Flow", "Focus Flow", false},
		{"please put on my gym playlist", "gym", false},
		{"play my playlist called Road Trip", "Road Trip", false},
		{"play my Discover Weekly playlist on shuffle", "Discover Weekly", true},
	}
	for _, c := range cases {
		got, ok := tl.Extract(c.text)
		if !ok || got.Name != c.name || got.Shuffle != c.shuffle {
			t.Errorf("Extract(%q) = %+v %v, want name %q shuffle %v", c.text, got, ok, c.name, c.shuffle)
		}
	}
	for _, text := range []string{"play some jazz", "what time is it", "shuffle the music", "play my liked songs", "play my playlist"} {
		if _, ok := tl.Extract(text); ok {
			t.Errorf("Extract(%q) accepted a request that names no playlist", text)
		}
	}
}

func TestShuffleTool(t *testing.T) {
	r := newLibRig(t, 0)
	tl := spotifytools.NewShuffle(r.svc)
	on, off := true, false
	out, err := tl.Handler(t.Context(), spotifytools.ShuffleIn{On: &on})
	if err != nil || !out.On || tl.Speak(out) != "Shuffle is on." {
		t.Fatalf("on = %+v, %v", out, err)
	}
	if !r.api.calledQuery("PUT /me/player/shuffle?") || !r.api.calledQuery("state=true") {
		t.Fatalf("calls = %v", r.api.raw)
	}
	out, _ = tl.Handler(t.Context(), spotifytools.ShuffleIn{On: &off})
	if out.On || tl.Speak(out) != "Shuffle is off." || !r.api.calledQuery("state=false") {
		t.Fatalf("off = %+v (%v)", out, r.api.raw)
	}
	// An absent flag means on.
	if out, _ = tl.Handler(t.Context(), spotifytools.ShuffleIn{}); !out.On {
		t.Fatal("empty input did not turn shuffle on")
	}
	cases := map[string]bool{
		"shuffle the music": true, "turn shuffle on": true, "shuffle my songs": true,
		"turn off shuffle": false, "stop shuffling": false, "turn shuffle off": false,
	}
	for text, want := range cases {
		got, ok := tl.Extract(text)
		if !ok || got.On == nil || *got.On != want {
			t.Errorf("Extract(%q) = %+v %v, want on=%v", text, got, ok, want)
		}
	}
	for _, text := range []string{"shuffle my running playlist", "play something", "shuffle the deck of cards"} {
		if _, ok := tl.Extract(text); ok {
			t.Errorf("Extract(%q) was accepted", text)
		}
	}
}

func TestRepeatTool(t *testing.T) {
	r := newLibRig(t, 0)
	tl := spotifytools.NewRepeat(r.svc)
	cases := []struct {
		text string
		mode string
		say  string
	}{
		{"repeat this song", "track", "Repeating this song."},
		{"put this song on repeat", "track", "Repeating this song."},
		{"repeat the playlist", "context", "Repeating the playlist."},
		{"repeat this album", "context", "Repeating the playlist."},
		{"turn on repeat", "context", "Repeating the playlist."},
		{"turn off repeat", "off", "Repeat is off."},
		{"stop repeating the song", "off", "Repeat is off."},
	}
	for _, c := range cases {
		in, ok := tl.Extract(c.text)
		if !ok || in.Mode != c.mode {
			t.Errorf("Extract(%q) = %+v %v, want %s", c.text, in, ok, c.mode)
			continue
		}
		out, err := tl.Handler(t.Context(), in)
		if err != nil || tl.Speak(out) != c.say {
			t.Errorf("%q: out=%+v err=%v speak=%q", c.text, out, err, tl.Speak(out))
		}
		if !r.api.calledQuery("/me/player/repeat?") || !r.api.calledQuery("state="+c.mode) {
			t.Errorf("%q: calls = %v", c.text, r.api.raw)
		}
	}
	// A bare "repeat that" belongs to assistant.repeat, not to music.
	for _, text := range []string{"repeat that", "can you repeat that please", "repeat after me", "say that again"} {
		if _, ok := tl.Extract(text); ok {
			t.Errorf("Extract(%q) stole an assistant.repeat phrase", text)
		}
	}
	if _, err := tl.Handler(t.Context(), spotifytools.RepeatIn{Mode: "forever"}); err == nil {
		t.Fatal("accepted an unknown mode")
	}
}

func TestPreviousTool(t *testing.T) {
	r := newLibRig(t, 0)
	tl := spotifytools.NewPrevious(r.svc)
	out, err := tl.Handler(t.Context(), spotifytools.PreviousIn{})
	if err != nil || tl.Speak(out) != "Previous track." {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if !r.api.calledWith("POST /me/player/previous") {
		t.Fatalf("calls = %v", r.api.calls)
	}
}

func TestPreviousIsNotAFocusIntent(t *testing.T) {
	// The shared intent vocabulary is fixed by the spec (stop, pause, resume,
	// next, cancel, snooze); "previous" has no meaning for a Ring or a reply,
	// so it stays a plain Tool and music declares no intent for it.
	for _, intent := range spotifytools.NewModule().Intents() {
		if string(intent) == "previous" {
			t.Fatal("music declares a previous intent")
		}
	}
	if _, ok := focus.ParseIntent("previous"); ok {
		t.Fatal("previous became a shared intent; the spec fixes the list")
	}
}

func TestQueueAddTool(t *testing.T) {
	r := newLibRig(t, 0)
	r.api.search = `{"tracks":{"items":[{"name":"Creep","uri":"spotify:track:creep","artists":[{"name":"Radiohead"}]}]}}`
	tl := spotifytools.NewQueueAdd(r.svc)
	in, ok := tl.Extract("add Creep by Radiohead to the queue")
	if !ok || in.Query != "Creep Radiohead" {
		t.Fatalf("Extract = %+v %v", in, ok)
	}
	out, err := tl.Handler(t.Context(), in)
	if err != nil || tl.Speak(out) != "Added Creep by Radiohead to the queue." {
		t.Fatalf("out = %+v, %v (%q)", out, err, tl.Speak(out))
	}
	if !r.api.calledWith("POST /me/player/queue") {
		t.Fatalf("calls = %v", r.api.calls)
	}
	if r.focus.Held(focus.Music) {
		t.Fatal("queueing claimed Music Focus")
	}
	for _, text := range []string{"queue up Karma Police", "add Karma Police to my queue", "queue Karma Police next"} {
		if got, ok := tl.Extract(text); !ok || !strings.Contains(got.Query, "Karma Police") {
			t.Errorf("Extract(%q) = %+v %v", text, got, ok)
		}
	}
	if _, ok := tl.Extract("add milk to the shopping list"); ok {
		t.Error("accepted a shopping list request")
	}
	if _, err := tl.Handler(t.Context(), spotifytools.QueueAddIn{}); err == nil {
		t.Fatal("queued an empty request")
	}
}

func TestLikeTool(t *testing.T) {
	r := newLibRig(t, 0)
	tl := spotifytools.NewLike(r.svc)
	if _, err := tl.Handler(t.Context(), spotifytools.LikeIn{}); err == nil {
		t.Fatal("liked with nothing playing")
	} else if te, ok := err.(*tool.ToolError); !ok || te.Say != "Nothing is playing to like." {
		t.Fatalf("err = %v", err)
	}
	r.api.mu.Lock()
	r.api.player = `{"is_playing":true,"device":{"name":"Clanka"},"item":{"id":"abc","type":"track","name":"Creep","artists":[{"name":"Radiohead"}],"album":{"name":"Pablo Honey"}}}`
	r.api.mu.Unlock()
	out, err := tl.Handler(t.Context(), spotifytools.LikeIn{})
	if err != nil || tl.Speak(out) != "Saved Creep by Radiohead to your liked songs." {
		t.Fatalf("out = %+v, %v (%q)", out, err, tl.Speak(out))
	}
	if !r.api.calledWith("PUT /me/tracks") {
		t.Fatalf("calls = %v", r.api.calls)
	}
}

func TestLibraryToolsWithoutALibrarySayTheyAreUnavailable(t *testing.T) {
	svc := spotifytools.NewService(nil, nil)
	_, err := spotifytools.NewPrevious(svc).Handler(t.Context(), spotifytools.PreviousIn{})
	if te, ok := err.(*tool.ToolError); !ok || te.Say == "" {
		t.Fatalf("err = %v", err)
	}
}

func TestLibraryToolsAreActAndRegisterTogether(t *testing.T) {
	r := newLibRig(t, 0)
	reg := tool.NewRegistry(5e9)
	if err := spotifytools.RegisterTools(reg, r.svc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		spotifytools.PlayPlaylistName, spotifytools.ShuffleName, spotifytools.RepeatName,
		spotifytools.PreviousName, spotifytools.QueueAddName, spotifytools.LikeName,
	} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if info.Permission != tool.PermissionAct {
			t.Errorf("%s permission = %v, want act", name, info.Permission)
		}
	}
}

func TestQueueAddExtractRejectsRequestsThatNameNoSong(t *testing.T) {
	tl := spotifytools.NewQueueAdd(newLibRig(t, 0).svc)
	for _, text := range []string{"add this to my queue", "queue up something", "add a song to the queue"} {
		if _, ok := tl.Extract(text); ok {
			t.Errorf("Extract(%q) accepted a request with no song", text)
		}
	}
}
