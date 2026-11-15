package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// stubSearchProvider returns one scripted result list; it never reaches a
// network. The real SearXNG mapping is covered in web_test.go.
type stubSearchProvider struct {
	results []Result
	err     error
}

func (p *stubSearchProvider) Search(context.Context, string, int) ([]Result, error) {
	return p.results, p.err
}

// stubFetcher records the URL the Tool allowed through to the reader.
type stubFetcher struct {
	mu   sync.Mutex
	urls []string
	page Page
	err  error
}

func (f *stubFetcher) Fetch(_ context.Context, rawURL string) (Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, rawURL)
	return f.page, f.err
}

func (f *stubFetcher) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// requestCtx is one turn's Tool context: identity plus the utterance web.fetch
// checks a user-named URL against.
func requestCtx(id, userText string) context.Context {
	return tool.WithRequest(context.Background(), tool.Request{
		ID: id, Path: tool.PathAgent, Device: tool.DevicePi, UserText: userText,
	})
}

func TestFetchAcceptsASearchURLInALaterTurn(t *testing.T) {
	provider := &stubSearchProvider{results: []Result{
		{Title: "Match report", URL: "https://news.example.test/report"},
	}}
	reader := &stubFetcher{page: Page{URL: "https://news.example.test/report", Text: "City won 2-1."}}
	provenance := NewProvenance()
	search := New(DefaultConfig(), provider, provenance, discardLogger())
	fetch := NewFetch(DefaultFetchConfig(), reader, provenance, discardLogger())

	if _, err := search.Handler(requestCtx("req-turn-1", "who won the match"), In{Query: "who won the match"}); err != nil {
		t.Fatalf("search: %v", err)
	}

	// A later turn: different request ID and no URL in the utterance. The
	// documented "read me that article" follow-up still works.
	page, err := fetch.Handler(requestCtx("req-turn-2", "read me the top one"), FetchIn{URL: "https://news.example.test/report"})
	if err != nil {
		t.Fatalf("later-turn fetch: %v", err)
	}
	if page.Text != "City won 2-1." {
		t.Fatalf("page = %+v", page)
	}
	if got := reader.fetched(); len(got) != 1 || got[0] != "https://news.example.test/report" {
		t.Fatalf("reader saw %v", got)
	}
}

func TestFetchAcceptsAURLTheUserNamed(t *testing.T) {
	reader := &stubFetcher{page: Page{URL: "https://news.example.test/report", Text: "City won 2-1."}}
	tl := NewFetch(DefaultFetchConfig(), reader, NewProvenance(), discardLogger())

	page, err := tl.Handler(
		requestCtx("req-1", "please read https://news.example.test/report to me"),
		FetchIn{URL: "https://news.example.test/report"},
	)
	if err != nil {
		t.Fatalf("user-named fetch: %v", err)
	}
	if page.Text != "City won 2-1." {
		t.Fatalf("page = %+v", page)
	}
}

func TestFetchRefusesAnUnvettedURLBeforeDNS(t *testing.T) {
	// A public host is scripted so the fetcher could resolve it; the refusal
	// must happen before the resolver or the dialer is touched.
	fixture := newFetchFixture(t, DefaultFetchConfig(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the unvetted URL reached the fixture server")
	}), map[string][]netip.Addr{"evil.example.test": publicV4()})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tl := NewFetch(DefaultFetchConfig(), fixture.fetcher, NewProvenance(), logger)

	_, err := tl.Handler(requestCtx("req-injected", "read me the top result"),
		FetchIn{URL: "https://evil.example.test/?d=shopping-list"})
	if err == nil {
		t.Fatal("unvetted URL was fetched")
	}
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) || toolErr.Say != fetchUnvetted {
		t.Fatalf("error = %v, want the fixed provenance line", err)
	}
	if got := tool.FailureMessage(err); strings.Contains(got, "evil.example.test") {
		t.Fatalf("failure message leaked the URL: %q", got)
	}
	if calls := fixture.resolver.callCount("evil.example.test"); calls != 0 {
		t.Fatalf("resolver was called %d times before the provenance refusal", calls)
	}
	if dials := fixture.dialer.recorded(); len(dials) != 0 {
		t.Fatalf("dialer was called before the provenance refusal: %v", dials)
	}
	// Refusal diagnostics must not retain the URL or its private query text.
	if strings.Contains(logs.String(), "evil.example.test") || strings.Contains(logs.String(), "shopping-list") {
		t.Fatalf("debug log leaked the refused URL or private query: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "web fetch refused an unvetted URL") {
		t.Fatalf("debug log omitted the refusal event: %s", logs.String())
	}
}

func TestProvenanceMatchesTheSameResourceOnly(t *testing.T) {
	provenance := newProvenance(time.Minute, provenanceMaxEntries, func() time.Time {
		return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	})
	provenance.Record([]string{"https://News.Example.Test:443/report?x=1#section"})

	allowed := []string{
		"https://news.example.test/report?x=1",
		"HTTPS://NEWS.EXAMPLE.TEST/report?x=1",
		"https://news.example.test/report?x=1#other",
	}
	for _, rawURL := range allowed {
		if !provenance.Allows(rawURL, "") {
			t.Errorf("Allows(%q) = false, want true", rawURL)
		}
	}
	refused := []string{
		"https://news.example.test/report?x=2", // a different query is a different exfiltration target
		"http://news.example.test/report?x=1",  // a scheme change is a different destination
		"https://news.example.test/report/x=1", // a different path
		"https://news.example.test:8443/report?x=1",
		"https://evil.example.test/report?x=1",
	}
	for _, rawURL := range refused {
		if provenance.Allows(rawURL, "") {
			t.Errorf("Allows(%q) = true, want false", rawURL)
		}
	}
}

func TestProvenanceCacheIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	provenance := newProvenance(provenanceWindow, provenanceMaxEntries, func() time.Time { return now })
	urls := make([]string, 0, provenanceMaxEntries+44)
	for i := 0; i < provenanceMaxEntries+44; i++ {
		urls = append(urls, fmt.Sprintf("https://example.test/article/%d", i))
		now = now.Add(time.Millisecond)
	}
	provenance.Record(urls)

	provenance.mu.Lock()
	size := len(provenance.seen)
	provenance.mu.Unlock()
	if size != provenanceMaxEntries {
		t.Fatalf("cache holds %d entries, want the %d-entry bound", size, provenanceMaxEntries)
	}
	if !provenance.Allows(urls[len(urls)-1], "") {
		t.Fatal("the newest URL was evicted")
	}
	if provenance.Allows(urls[0], "") {
		t.Fatal("the oldest URL survived past the bound")
	}
}

func TestProvenanceWindowExpires(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	provenance := newProvenance(30*time.Minute, provenanceMaxEntries, func() time.Time { return now })
	provenance.Record([]string{"https://example.test/article"})

	now = now.Add(30 * time.Minute)
	if !provenance.Allows("https://example.test/article", "") {
		t.Fatal("URL expired inside the window")
	}
	now = now.Add(time.Second)
	if provenance.Allows("https://example.test/article", "") {
		t.Fatal("URL survived past the window")
	}
}

// echoSearchProvider turns each query into one result URL, so the concurrent
// test can drive many distinct provenance entries through the real Tools.
type echoSearchProvider struct{}

func (echoSearchProvider) Search(_ context.Context, query string, _ int) ([]Result, error) {
	return []Result{{URL: "https://example.test/search/" + query}}, nil
}

func TestProvenanceIsSafeForConcurrentSearchAndFetch(t *testing.T) {
	provenance := NewProvenance()
	reader := &stubFetcher{page: Page{Text: "read"}}
	search := New(DefaultConfig(), echoSearchProvider{}, provenance, discardLogger())
	fetch := NewFetch(DefaultFetchConfig(), reader, provenance, discardLogger())

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				query := fmt.Sprintf("worker-%d-%d", worker, i)
				if _, err := search.Handler(requestCtx("search", query), In{Query: query}); err != nil {
					t.Errorf("search: %v", err)
					return
				}
				url := "https://example.test/search/" + query
				if _, err := fetch.Handler(requestCtx("fetch", "read the top result"), FetchIn{URL: url}); err != nil {
					t.Errorf("fetch of a just-returned URL: %v", err)
					return
				}
				if _, err := fetch.Handler(requestCtx("fetch", "read the top result"),
					FetchIn{URL: "https://unvetted.example.test/"}); err == nil {
					t.Error("an unvetted URL was fetched")
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}
