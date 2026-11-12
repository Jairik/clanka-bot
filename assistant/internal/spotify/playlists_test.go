package spotify_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

const (
	pageOne = `{"items":[{"name":"Running","uri":"spotify:playlist:run"},null,{"name":"Chill Vibes","uri":"spotify:playlist:chill"}],"next":"https://api.spotify.com/v1/me/playlists?offset=3&limit=50"}`
	pageTwo = `{"items":[{"name":"Focus","uri":"spotify:playlist:focus"}],"next":null}`
)

// scriptedAPI is an in-memory API for the cache tests that need control over
// timing; the httptest fake covers the real client path.
type scriptedAPI struct {
	mu    sync.Mutex
	pages map[string]string
	err   error
	gate  chan struct{} // when set, every Get waits for it to close
	calls atomic.Int32
	seen  []url.Values
}

func (s *scriptedAPI) Get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	s.calls.Add(1)
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, q)
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.pages[q.Get("offset")]), nil
}
func (s *scriptedAPI) Post(context.Context, string, url.Values, []byte) ([]byte, error) {
	return nil, errors.New("unexpected POST")
}
func (s *scriptedAPI) Put(context.Context, string, url.Values, []byte) ([]byte, error) {
	return nil, errors.New("unexpected PUT")
}

func (s *scriptedAPI) set(err error, pages map[string]string) {
	s.mu.Lock()
	s.err, s.pages = err, pages
	s.mu.Unlock()
}

// syncBuffer is a log sink safe for the refresh goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeTicker struct {
	ch       chan time.Time
	interval atomic.Int64
	stopped  atomic.Bool
}

func newFakeTicker() *fakeTicker { return &fakeTicker{ch: make(chan time.Time)} }

func (f *fakeTicker) factory(d time.Duration) (<-chan time.Time, func()) {
	f.interval.Store(int64(d))
	return f.ch, func() { f.stopped.Store(true) }
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRefreshPagesThroughTheRealClientAndCachesNames(t *testing.T) {
	f := newAuthFixture(t)
	f.beginAndComplete(t)
	api := &fakeWebAPI{playlists: map[string]string{"0": pageOne, "3": pageTwo}}
	f.fake.mu.Lock()
	f.fake.api = api.handler(t)
	f.fake.mu.Unlock()

	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: f.auth.Client()})
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got, want := strings.Join(cache.Names(), ","), "Chill Vibes,Focus,Running"; got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	pl, ok := cache.Lookup("Running")
	if !ok || pl.URI != "spotify:playlist:run" {
		t.Fatalf("Lookup = %+v, %v", pl, ok)
	}
	if !cache.Loaded() {
		t.Fatal("cache not loaded after a refresh")
	}
	calls := api.all()
	if len(calls) != 2 || !strings.Contains(calls[0], "limit=50") || !strings.Contains(calls[1], "offset=3") {
		t.Fatalf("paging calls = %v", calls)
	}
}

func TestRefreshStopsAtTheBoundedPageCount(t *testing.T) {
	api := &scriptedAPI{pages: map[string]string{}}
	endless := func(off int) string {
		return `{"items":[{"name":"P` + string(rune('A'+off)) + `","uri":"spotify:playlist:` + string(rune('a'+off)) + `"}],"next":"more"}`
	}
	for i := 0; i < 10; i++ {
		api.pages[strconv.Itoa(i)] = endless(i)
	}
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: api, MaxPages: 3, PageSize: 1})
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if n := api.calls.Load(); n != 3 {
		t.Fatalf("pages fetched = %d, want the bound of 3", n)
	}
	if len(cache.Names()) != 3 {
		t.Fatalf("names = %v, want the 3 fetched pages kept", cache.Names())
	}
}

func TestFailedRefreshKeepsTheOldCacheAndNeverLogsATokenOrBody(t *testing.T) {
	api := &scriptedAPI{pages: map[string]string{"0": pageTwo}}
	var logs syncBuffer
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{
		API: api, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	api.set(errors.New("boom Bearer sk-secret-token"), nil)
	if err := cache.Refresh(t.Context()); err == nil {
		t.Fatal("failed refresh returned nil")
	}
	// The background loop logs through refreshLogged; drive it with a tick.
	tick := newFakeTicker()
	logged := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{
		API: api, Logger: slog.New(slog.NewTextHandler(&logs, nil)), NewTicker: tick.factory,
	})
	logged.Start(t.Context())
	eventually(t, "failure logged", func() bool { return strings.Contains(logs.String(), "refresh failed") })
	logged.Close()
	if got := strings.Join(cache.Names(), ","); got != "Focus" {
		t.Fatalf("names after failure = %q, want the old cache", got)
	}
	// A page that fails halfway must not leave a half-built cache either.
	api.set(nil, map[string]string{"0": pageOne})
	api.mu.Lock()
	api.pages["3"] = "not json"
	api.mu.Unlock()
	if err := cache.Refresh(t.Context()); err == nil {
		t.Fatal("malformed page returned nil")
	}
	if got := strings.Join(cache.Names(), ","); got != "Focus" {
		t.Fatalf("names after partial failure = %q, want the old cache", got)
	}
	if strings.Contains(logs.String(), "sk-secret-token") {
		t.Fatalf("a token reached the log: %s", logs.String())
	}
}

func TestStartDoesNotBlockAndRefreshesAtStartupAndOnEveryTick(t *testing.T) {
	api := &scriptedAPI{pages: map[string]string{"0": pageTwo}, gate: make(chan struct{})}
	tick := newFakeTicker()
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: api, NewTicker: tick.factory})

	done := make(chan struct{})
	go func() { cache.Start(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start blocked on the refresh")
	}
	t.Cleanup(cache.Close)
	if cache.Loaded() {
		t.Fatal("cache loaded before the API answered")
	}
	close(api.gate)
	eventually(t, "startup refresh", cache.Loaded)
	if got := time.Duration(tick.interval.Load()); got != 6*time.Hour {
		t.Fatalf("ticker interval = %v, want 6h", got)
	}

	// The playlist set changes; one tick refreshes it.
	api.set(nil, map[string]string{"0": `{"items":[{"name":"Late Night","uri":"spotify:playlist:late"}],"next":null}`})
	tick.ch <- time.Now()
	eventually(t, "tick refresh", func() bool { return strings.Join(cache.Names(), ",") == "Late Night" })
}

func TestCloseCancelsAnInFlightRefreshAndJoins(t *testing.T) {
	api := &scriptedAPI{pages: map[string]string{"0": pageTwo}, gate: make(chan struct{})}
	tick := newFakeTicker()
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: api, NewTicker: tick.factory})
	cache.Start(t.Context())
	eventually(t, "refresh in flight", func() bool { return api.calls.Load() == 1 })

	closed := make(chan struct{})
	go func() { cache.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel and join the refresh")
	}
	if !tick.stopped.Load() {
		t.Fatal("Close left the ticker running")
	}
	cache.Close() // idempotent
	if cache.Loaded() {
		t.Fatal("a cancelled refresh published a cache")
	}
}

func TestLookupIsEmptyBeforeTheFirstRefresh(t *testing.T) {
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{API: &scriptedAPI{}})
	if _, ok := cache.Lookup("Running"); ok || len(cache.Names()) != 0 || cache.Loaded() {
		t.Fatal("empty cache reported content")
	}
}
