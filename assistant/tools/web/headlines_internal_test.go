package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// feedURL is the fixed fake host the headlines fixtures serve and resolve. The
// scriptedResolver maps it to a public literal, so the SSRF guard accepts it
// and the recordingDialer connects to the local server.
const feedURL = "http://feed.example.test/feed"

// headlinesTool builds news.headlines over a fixture fetcher and an explicit
// feed list. The name matcher is nil so the fallback normalised-exact compare
// resolves any spoken source, which is what the offline tests want.
func headlinesTool(fetcher *fetcher, feeds []FeedConfig) tool.Tool[HeadlinesIn, HeadlinesOut] {
	return NewHeadlines(Config{Feeds: feeds}, fetcher, NewProvenance(), discardLogger(), nil)
}

// sayOf unwraps the spoken line from a headlines refusal.
func sayOf(t *testing.T, err error) string {
	t.Helper()
	var te *tool.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not a *tool.ToolError", err)
	}
	return te.Say
}

// buildRSS emits n RSS 2.0 items with strictly increasing pubDates, so the
// newest-first sort the handler applies is deterministic.
func buildRSS(n int, base time.Time) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0"><channel><title>Example News</title><link>https://example.com</link>`)
	for i := 1; i <= n; i++ {
		published := base.Add(time.Hour * time.Duration(i))
		b.WriteString(fmt.Sprintf(
			`<item><title>Headline %d</title><link>https://example.com/%d</link><pubDate>%s</pubDate></item>`,
			i, i, published.Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// buildAtom emits n Atom entries with strictly increasing updated dates.
func buildAtom(n int, base time.Time) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom"><title>Example News</title>`)
	for i := 1; i <= n; i++ {
		updated := base.Add(time.Hour * time.Duration(i))
		b.WriteString(fmt.Sprintf(
			`<entry><title>Headline %d</title><link href="https://example.com/%d"/><updated>%s</updated></entry>`,
			i, i, updated.Format(time.RFC3339)))
	}
	b.WriteString(`</feed>`)
	return b.String()
}

func TestHeadlinesReadsRSSFeed(t *testing.T) {
	body := buildRSS(7, time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC))
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			fmt.Fprint(w, body)
		}), map[string][]netip.Addr{"feed.example.test": publicV4()})

	feeds := []FeedConfig{{Name: "BBC", URL: feedURL}}
	tl := headlinesTool(fixture.fetcher, feeds)
	out, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Source != "BBC" {
		t.Errorf("source = %q, want BBC", out.Source)
	}
	// Newest five of seven, in descending date order.
	want := []string{"Headline 7", "Headline 6", "Headline 5", "Headline 4", "Headline 3"}
	if len(out.Items) != len(want) {
		t.Fatalf("items = %d, want %d", len(out.Items), len(want))
	}
	for i, wantTitle := range want {
		if out.Items[i].Title != wantTitle {
			t.Errorf("item %d title = %q, want %q", i, out.Items[i].Title, wantTitle)
		}
		if out.Items[i].Source != "BBC" {
			t.Errorf("item %d source = %q, want BBC", i, out.Items[i].Source)
		}
		if out.Items[i].Link == "" {
			t.Errorf("item %d has no link", i)
		}
	}
}

func TestHeadlinesReadsAtomFeed(t *testing.T) {
	body := buildAtom(7, time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC))
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/atom+xml")
			fmt.Fprint(w, body)
		}), map[string][]netip.Addr{"feed.example.test": publicV4()})

	feeds := []FeedConfig{{Name: "Reuters", URL: feedURL}}
	tl := headlinesTool(fixture.fetcher, feeds)
	out, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Source != "Reuters" {
		t.Errorf("source = %q, want Reuters", out.Source)
	}
	want := []string{"Headline 7", "Headline 6", "Headline 5", "Headline 4", "Headline 3"}
	if len(out.Items) != len(want) {
		t.Fatalf("items = %d, want %d", len(out.Items), len(want))
	}
	for i, wantTitle := range want {
		if out.Items[i].Title != wantTitle {
			t.Errorf("item %d title = %q, want %q", i, out.Items[i].Title, wantTitle)
		}
		if out.Items[i].Link == "" {
			t.Errorf("item %d has no link", i)
		}
	}
}

func TestHeadlinesRefusesMalformedFeed(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, "<html><body>not a feed</body></html>")
		}), map[string][]netip.Addr{"feed.example.test": publicV4()})

	tl := headlinesTool(fixture.fetcher, []FeedConfig{{Name: "BBC", URL: feedURL}})
	_, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err == nil {
		t.Fatal("expected a refusal for a malformed feed")
	}
	if got := sayOf(t, err); got != headlinesUnavailable {
		t.Errorf("say = %q, want %q", got, headlinesUnavailable)
	}
}

func TestHeadlinesRefusesOversizeFeed(t *testing.T) {
	body := strings.Repeat("x", 600*1024)
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			fmt.Fprint(w, body)
		}), map[string][]netip.Addr{"feed.example.test": publicV4()})

	tl := headlinesTool(fixture.fetcher, []FeedConfig{{Name: "BBC", URL: feedURL}})
	_, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err == nil {
		t.Fatal("expected a refusal for an oversize feed")
	}
	if got := sayOf(t, err); got != headlinesUnavailable {
		t.Errorf("say = %q, want %q", got, headlinesUnavailable)
	}
}

// TestHeadlinesFeedBytesRejectsOversize proves the SSRF-guarded feed reader
// enforces the 512 KiB bound independent of the handler's graceful refusal.
func TestHeadlinesFeedBytesRejectsOversize(t *testing.T) {
	body := strings.Repeat("x", 600*1024)
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			fmt.Fprint(w, body)
		}), map[string][]netip.Addr{"feed.example.test": publicV4()})

	_, _, err := fixture.fetcher.fetchFeedBytes(context.Background(), feedURL, maxFeedBytes)
	if !errors.Is(err, errFetchTooLarge) {
		t.Fatalf("fetchFeedBytes error = %v, want errFetchTooLarge", err)
	}
}

func TestHeadlinesFeedBytesBlocksPrivateAddress(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		map[string][]netip.Addr{"feed.example.test": {netip.MustParseAddr("192.168.1.20")}})

	_, _, err := fixture.fetcher.fetchFeedBytes(context.Background(), feedURL, maxFeedBytes)
	if !errors.Is(err, errBlockedAddress) {
		t.Fatalf("fetchFeedBytes error = %v, want errBlockedAddress", err)
	}
	// The guard must reject before any dial, so no connection is attempted to
	// the private literal.
	if got := fixture.dialer.recorded(); len(got) != 0 {
		t.Errorf("dial targets = %v, want none (private address must not be dialled)", got)
	}
}

func TestHeadlinesRefusesPrivateAddressFeed(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		map[string][]netip.Addr{"feed.example.test": {netip.MustParseAddr("192.168.1.20")}})

	tl := headlinesTool(fixture.fetcher, []FeedConfig{{Name: "BBC", URL: feedURL}})
	_, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err == nil {
		t.Fatal("expected a refusal for a private-address feed")
	}
	if got := sayOf(t, err); got != headlinesUnavailable {
		t.Errorf("say = %q, want %q", got, headlinesUnavailable)
	}
	if got := fixture.dialer.recorded(); len(got) != 0 {
		t.Errorf("dial targets = %v, want none (private address must not be dialled)", got)
	}
}

func TestHeadlinesCachesFeedReuse(t *testing.T) {
	body := buildRSS(7, time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC))
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			fmt.Fprint(w, body)
		}), map[string][]netip.Addr{"feed.example.test": publicV4()})

	tl := headlinesTool(fixture.fetcher, []FeedConfig{{Name: "BBC", URL: feedURL}})
	out1, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err != nil {
		t.Fatalf("first handle: %v", err)
	}
	out2, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	if len(out1.Items) != len(out2.Items) {
		t.Fatalf("second read returned %d items, first returned %d", len(out2.Items), len(out1.Items))
	}
	// The second read must come from the 15-minute cache, so the resolver is
	// consulted exactly once for the feed host.
	if calls := fixture.resolver.callCount("feed.example.test"); calls != 1 {
		t.Errorf("resolver calls = %d, want 1 (feed must be cached after the first read)", calls)
	}
}

func TestHeadlinesRefusesWhenNoFeedsConfigured(t *testing.T) {
	tl := headlinesTool(newFetcher(DefaultFetchConfig()), nil)
	_, err := tl.Handler(context.Background(), HeadlinesIn{})
	if err == nil {
		t.Fatal("expected a refusal when no feeds are configured")
	}
	if got := sayOf(t, err); got != headlinesNoFeeds {
		t.Errorf("say = %q, want %q", got, headlinesNoFeeds)
	}
}

func TestHeadlinesRefusesUnknownSource(t *testing.T) {
	// The body is never read because source resolution fails before any fetch.
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		map[string][]netip.Addr{"feed.example.test": publicV4()})

	tl := headlinesTool(fixture.fetcher, []FeedConfig{{Name: "BBC", URL: feedURL}})
	_, err := tl.Handler(context.Background(), HeadlinesIn{Source: "CNN"})
	if err == nil {
		t.Fatal("expected a refusal for an unknown source")
	}
	if got := sayOf(t, err); got != fmt.Sprintf(headlinesUnknownFeed, "CNN") {
		t.Errorf("say = %q, want %q", got, fmt.Sprintf(headlinesUnknownFeed, "CNN"))
	}
}

func TestHeadlinesExtractsSourceFromUtterance(t *testing.T) {
	// Extract names a feed only after "from", "on" or "via"; every other
	// phrasing reads every configured feed (empty source).
	cases := []struct {
		text   string
		source string
	}{
		{"headlines from BBC", "BBC"},
		{"news on technology", "technology"},
		{"read me the headlines via Reuters", "Reuters"},
		{"what's the news", ""},
		{"read me the headlines", ""},
		{"top stories from the BBC today", "the BBC today"},
	}
	for _, tt := range cases {
		in, ok := extractHeadlinesSource(tt.text)
		if !ok {
			t.Errorf("extract(%q) ok = false, want true", tt.text)
		}
		if in.Source != tt.source {
			t.Errorf("extract(%q).Source = %q, want %q", tt.text, in.Source, tt.source)
		}
	}
}
