package spotify

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// The playlist pages these tests serve are one page each: the warm wiring is
// the subject, not the paging the cache tests already cover.
const (
	warmTwoLists  = `{"items":[{"name":"Running","uri":"spotify:playlist:run"},{"name":"Chill Vibes","uri":"spotify:playlist:chill"}],"next":null}`
	warmNewList   = `{"items":[{"name":"Running","uri":"spotify:playlist:run"},{"name":"Chill Vibes","uri":"spotify:playlist:chill"},{"name":"Late Night","uri":"spotify:playlist:late"}],"next":null}`
	warmOtherList = `{"items":[{"name":"Late Night","uri":"spotify:playlist:late"}],"next":null}`
)

// warmAPI is the Web API seam the cache reads, in process. It serves one page
// per offset and can fail so a test can drive a failed refresh.
type warmAPI struct {
	mu    sync.Mutex
	pages map[string]string
	err   error
	gets  atomic.Int32
}

func (w *warmAPI) Get(_ context.Context, _ string, q url.Values) ([]byte, error) {
	w.gets.Add(1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	return []byte(w.pages[q.Get("offset")]), nil
}

func (w *warmAPI) Post(context.Context, string, url.Values, []byte) ([]byte, error) {
	return nil, errors.New("unexpected POST")
}

func (w *warmAPI) Put(context.Context, string, url.Values, []byte) ([]byte, error) {
	return nil, errors.New("unexpected PUT")
}

func (w *warmAPI) set(pages map[string]string, err error) {
	w.mu.Lock()
	w.pages, w.err = pages, err
	w.mu.Unlock()
}

// warmEmbedder gives each playlist a distinct axis, so a lookup can score a
// near miss against a warmed candidate, and records every batch it is asked
// for so a test can prove what reached the embed service. fail makes every
// batch fail, which is how the warm-failure test drives an unavailable service.
type warmEmbedder struct {
	mu      sync.Mutex
	batches [][]string
	fail    bool
}

func (e *warmEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.batches = append(e.batches, append([]string(nil), texts...))
	fail := e.fail
	e.mu.Unlock()
	if fail {
		return nil, errors.New("embed service unavailable")
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 3)
		switch lower := strings.ToLower(text); {
		case strings.Contains(lower, "run"):
			v[0] = 1
		case strings.Contains(lower, "chill"):
			v[1] = 1
		default:
			v[2] = 1
		}
		out[i] = v
	}
	return out, nil
}

func (e *warmEmbedder) setFail(fail bool) {
	e.mu.Lock()
	e.fail = fail
	e.mu.Unlock()
}

func (e *warmEmbedder) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.batches)
}

func (e *warmEmbedder) batch(i int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.batches[i]...)
}

// warmLogs is a log sink safe for the refresh goroutine and the test.
type warmLogs struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *warmLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *warmLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// warmTicker lets a test drive a refresh without waiting six hours.
type warmTicker struct{ ch chan time.Time }

func newWarmTicker() *warmTicker { return &warmTicker{ch: make(chan time.Time)} }

func (t *warmTicker) factory(time.Duration) (<-chan time.Time, func()) {
	return t.ch, func() {}
}

// countWarm counts calls around the production hook, so a test can tell "the
// cache never called the hook" from "the hook found nothing to warm".
func countWarm(calls *atomic.Int32, hook func(context.Context, []string)) func(context.Context, []string) {
	return func(ctx context.Context, names []string) {
		hook(ctx, names)
		calls.Add(1)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

func sameBatch(got, want []string) bool {
	return strings.Join(got, "|") == strings.Join(want, "|")
}

// TestPlaylistRefreshWarmsTheMatcherOnceAndOnlyWithNewNames is the acceptance
// test for the wiring: the production hook (warmNameMatcher over the shared
// matcher) runs once per successful refresh with the loaded names, embeds
// exactly the names the cache is missing, and leaves the next lookup with only
// the query to embed.
func TestPlaylistRefreshWarmsTheMatcherOnceAndOnlyWithNewNames(t *testing.T) {
	api := &warmAPI{pages: map[string]string{"0": warmTwoLists}}
	embedder := &warmEmbedder{}
	matcher := namematch.New(embedder, namematch.Config{Threshold: func() float64 { return 0.5 }})
	tick := newWarmTicker()
	var hooked atomic.Int32
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{
		API:       api,
		NewTicker: tick.factory,
		OnRefresh: countWarm(&hooked, warmNameMatcher(matcher)),
	})
	cache.Start(context.Background())
	t.Cleanup(cache.Close)

	// One refresh, one warm batch, holding the loaded names normalised.
	waitFor(t, "the warm after the startup refresh", func() bool { return hooked.Load() == 1 })
	if n := embedder.calls(); n != 1 {
		t.Fatalf("embed calls after the startup refresh = %d, want 1", n)
	}
	if got := embedder.batch(0); !sameBatch(got, []string{"chill vibes", "running"}) {
		t.Fatalf("warm batch = %v, want the two loaded names normalised", got)
	}

	// The cache is warm, so a near-miss lookup embeds only what it says.
	result := matcher.Match(context.Background(), "my running tunes", cache.Names())
	if !result.Matched() || result.Name != "Running" {
		t.Fatalf("Match after the warm = %+v, want Running", result)
	}
	if got := embedder.batch(1); !sameBatch(got, []string{"running tunes"}) {
		t.Fatalf("lookup batch = %v, want only the query", got)
	}

	// An unchanged library still calls the hook, but nothing is missing, so no
	// second embed request is made. This is what stops a six-hourly refresh
	// paying for a warm it does not need.
	tick.ch <- time.Now()
	waitFor(t, "the warm after an unchanged refresh", func() bool { return hooked.Load() == 2 })
	if n := embedder.calls(); n != 2 {
		t.Fatalf("embed calls after an unchanged refresh = %d, want 2: the warm is a no-op", n)
	}

	// An added playlist embeds that name alone; the cached ones are reused.
	api.set(map[string]string{"0": warmNewList}, nil)
	tick.ch <- time.Now()
	waitFor(t, "the warm after the library grew", func() bool { return hooked.Load() == 3 })
	if n := embedder.calls(); n != 3 {
		t.Fatalf("embed calls after the library grew = %d, want 3", n)
	}
	if got := embedder.batch(2); !sameBatch(got, []string{"late night"}) {
		t.Fatalf("warm batch for the new name = %v, want only the added name", got)
	}
}

// fakeWarmStore holds one refresh token so the module's real auth seam can
// reach an in-process token endpoint; nothing is written anywhere.
type fakeWarmStore struct{ token string }

func (s fakeWarmStore) Get(string) (string, bool) { return s.token, s.token != "" }
func (s fakeWarmStore) Metadata(name string) (secrets.Metadata, bool) {
	return secrets.Metadata{Name: name, Source: secrets.SourceNone, Set: s.token != ""}, true
}
func (s fakeWarmStore) Set(context.Context, string, string) (secrets.Metadata, error) {
	return secrets.Metadata{}, nil
}
func (s fakeWarmStore) Clear(context.Context, string) (secrets.Metadata, error) {
	return secrets.Metadata{}, nil
}

// TestModuleStartWarmsTheSharedNameMatcher runs the production wiring end to
// end: NewModule, BindConfig, Register and Start with a fake token endpoint and
// playlist page, so the cache the module builds really does hand a successful
// refresh to deps.Names through the OnRefresh hook.
func TestModuleStartWarmsTheSharedNameMatcher(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"Bearer","expires_in":3600}`))
		case "/me/playlists":
			pages.Add(1)
			_, _ = w.Write([]byte(warmTwoLists))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := spotify.DefaultConfig()
	cfg.ClientID = "0123456789abcdef0123456789abcdef"
	cfg.RedirectURI = "http://127.0.0.1:8082/spotify/callback"
	cfg.APIBaseURL = srv.URL
	cfg.TokenURL = srv.URL + "/token"

	embedder := &warmEmbedder{}
	matcher := namematch.New(embedder, namematch.Config{Threshold: func() float64 { return 0.5 }})
	module := NewModule()
	module.BindConfig(cfg)
	deps := tool.Deps{Secrets: fakeWarmStore{token: "refresh-token"}, Names: matcher}
	if err := module.Register(tool.NewRegistry(time.Second), deps); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := module.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer module.Close()

	waitFor(t, "the module's warm after its startup refresh", func() bool { return embedder.calls() == 1 })
	if pages.Load() == 0 {
		t.Fatal("the module never refreshed the playlist cache")
	}
	if got := embedder.batch(0); !sameBatch(got, []string{"chill vibes", "running"}) {
		t.Fatalf("module warm batch = %v, want the loaded playlist names", got)
	}
}

// TestWarmFailureDoesNotFailTheRefresh proves a broken embed service is
// invisible to the refresh: the names still load, nothing is spoken or logged
// as a refresh failure, and the first lookup still resolves from a cold batch
// exactly as it did before the warm existed.
func TestWarmFailureDoesNotFailTheRefresh(t *testing.T) {
	api := &warmAPI{pages: map[string]string{"0": warmTwoLists}}
	embedder := &warmEmbedder{fail: true}
	matcher := namematch.New(embedder, namematch.Config{Threshold: func() float64 { return 0.5 }})
	var logs warmLogs
	tick := newWarmTicker()
	cache := spotify.NewPlaylistCache(spotify.PlaylistCacheOptions{
		API:       api,
		Logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		NewTicker: tick.factory,
		OnRefresh: warmNameMatcher(matcher),
	})
	cache.Start(context.Background())
	t.Cleanup(cache.Close)

	waitFor(t, "the failed warm", func() bool { return embedder.calls() == 1 })
	if !cache.Loaded() {
		t.Fatal("a failed warm left the cache unloaded")
	}
	if got, want := strings.Join(cache.Names(), ","), "Chill Vibes,Running"; got != want {
		t.Fatalf("names after a failed warm = %q, want %q", got, want)
	}
	if logged := logs.String(); strings.Contains(logged, "refresh failed") {
		t.Fatalf("a warm failure was logged as a refresh failure: %s", logged)
	}

	// The refresh loop survives it: a tick still refreshes the library.
	api.set(map[string]string{"0": warmOtherList}, nil)
	gets := api.gets.Load()
	tick.ch <- time.Now()
	waitFor(t, "the next refresh after a failed warm", func() bool { return api.gets.Load() > gets })
	if got := strings.Join(cache.Names(), ","); got != "Late Night" {
		t.Fatalf("names after the next refresh = %q, want Late Night", got)
	}

	// And a lookup still works: with nothing cached the query embeds alongside
	// the candidates, which is the pre-warm path and never an error.
	embedder.setFail(false)
	result := matcher.Match(context.Background(), "late night", cache.Names())
	if !result.Matched() || result.Name != "Late Night" {
		t.Fatalf("Match after a failed warm = %+v, want Late Night", result)
	}
}
