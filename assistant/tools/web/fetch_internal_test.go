package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

// scriptedResolver answers a fixed set of hosts. A sequence lets a test model
// a rebinding resolver: the first answer is what prepare validates, the second
// would be what a second lookup sees.
type scriptedResolver struct {
	mu        sync.Mutex
	answers   map[string][]netip.Addr
	sequences map[string][][]netip.Addr
	calls     map[string]int
}

func newScriptedResolver(answers map[string][]netip.Addr) *scriptedResolver {
	if answers == nil {
		answers = map[string][]netip.Addr{}
	}
	return &scriptedResolver{
		answers:   answers,
		sequences: map[string][][]netip.Addr{},
		calls:     map[string]int{},
	}
}

func (r *scriptedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[host]++
	if sequence := r.sequences[host]; len(sequence) > 0 {
		answer := sequence[0]
		if len(sequence) > 1 {
			r.sequences[host] = sequence[1:]
		}
		return answer, nil
	}
	if answer, ok := r.answers[host]; ok {
		return answer, nil
	}
	return nil, fmt.Errorf("no scripted DNS answer for %s", host)
}

func (r *scriptedResolver) callCount(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// recordingDialer records the literal address the fetcher's guard asked for,
// then connects to the local fixture. It never dials the recorded address, so
// a test can prove the pin without touching a real public or private host.
type recordingDialer struct {
	mu        sync.Mutex
	targets   []string
	connectTo string
	fail      func(address string) bool
}

func (d *recordingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.targets = append(d.targets, address)
	fail := d.fail
	d.mu.Unlock()
	if fail != nil && fail(address) {
		return nil, errors.New("scripted dial failure")
	}
	return (&net.Dialer{}).DialContext(ctx, network, d.connectTo)
}

func (d *recordingDialer) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.targets...)
}

// fetchFixture is one local HTTP server plus the guarded fetcher under test.
type fetchFixture struct {
	fetcher  *fetcher
	server   *httptest.Server
	resolver *scriptedResolver
	dialer   *recordingDialer
}

func newFetchFixture(t *testing.T, cfg FetchConfig, handler http.Handler, answers map[string][]netip.Addr) *fetchFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	resolver := newScriptedResolver(answers)
	dialer := &recordingDialer{connectTo: server.Listener.Addr().String()}
	return &fetchFixture{
		fetcher:  newFetcherWith(cfg, resolver, dialer.DialContext),
		server:   server,
		resolver: resolver,
		dialer:   dialer,
	}
}

func publicV4() []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.216.34")} }

func TestPublicAddressPolicyBlocksNonPublicRanges(t *testing.T) {
	tests := []struct {
		address string
		public  bool
	}{
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"100.64.0.1", false},
		{"100.100.100.100", false},
		{"0.0.0.0", false},
		{"0.1.2.3", false},
		{"224.0.0.1", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"::", false},
		{"fc00::1", false},
		{"fd7a:115c:a1e0::1", false},
		{"fe80::1", false},
		{"ff02::1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		{"::ffff:100.64.0.1", false},
		{"::ffff:0:127.0.0.1", false},
		{"64:ff9b::7f00:1", false},
		{"2002:7f00:1::", false},
		{"2001:db8::1", false},
		{"93.184.216.34", true},
		{"8.8.8.8", true},
		{"2606:2800:220:1:248:1893:25c8:1946", true},
		{"2a00:1450:4001:80f::200e", true},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.address)
			if got := publicAddress(addr); got != tt.public {
				t.Fatalf("publicAddress(%s) = %v, want %v", tt.address, got, tt.public)
			}
		})
	}
}

func TestFetchURLPolicy(t *testing.T) {
	valid := []string{
		"https://example.com/story",
		"http://93.184.216.34/story",
		"https://[2606:2800:220:1:248:1893:25c8:1946]/story",
		"http://example.com:8080/page",
	}
	for _, raw := range valid {
		t.Run("valid "+raw, func(t *testing.T) {
			target, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse %s: %v", raw, err)
			}
			if err := checkFetchURL(target); err != nil {
				t.Fatalf("checkFetchURL(%s) = %v, want nil", raw, err)
			}
		})
	}

	invalid := []struct {
		name  string
		raw   string
		cause error
	}{
		{"empty", "", errBadFetchURL},
		{"file", "file:///etc/passwd", errBadFetchURL},
		{"data", "data:text/html,hello", errBadFetchURL},
		{"ftp", "ftp://example.com/file", errBadFetchURL},
		{"javascript", "javascript:alert(1)", errBadFetchURL},
		{"relative", "example.com/story", errBadFetchURL},
		{"parser disagreement", `http://example.com\@127.0.0.1/`, errBadFetchURL},
		{"credential user", "http://user@example.com/", errCredentials},
		{"credential pair", "http://user:pass@example.com/", errCredentials},
		{"loopback", "http://127.0.0.1/", errBlockedAddress},
		{"loopback v6", "http://[::1]/", errBlockedAddress},
		{"mapped loopback", "http://[::ffff:127.0.0.1]/", errBlockedAddress},
		{"translated loopback", "http://[::ffff:0:127.0.0.1]/", errBlockedAddress},
		{"private", "http://192.168.1.1/", errBlockedAddress},
		{"tailnet", "http://100.100.100.100/", errBlockedAddress},
		{"tailnet ula", "http://[fd7a:115c:a1e0::1]/", errBlockedAddress},
		{"metadata", "http://169.254.169.254/latest/meta-data/", errBlockedAddress},
		{"localhost", "http://localhost/", errBlockedAddress},
		{"localhost suffix", "http://foo.localhost/", errBlockedAddress},
		{"decimal ipv4", "http://2130706433/", errBlockedAddress},
		{"hex ipv4", "http://0x7f000001/", errBlockedAddress},
		{"short ipv4", "http://127.1/", errBlockedAddress},
		{"octal ipv4", "http://0177.0.0.1/", errBlockedAddress},
		{"port zero", "http://example.com:0/", errBadFetchURL},
		{"port too large", "http://example.com:99999/", errBadFetchURL},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			target, err := url.Parse(tt.raw)
			if err != nil {
				// An unparsable URL is already a bad address; that is the
				// same outcome the handler maps.
				if !errors.Is(tt.cause, errBadFetchURL) {
					t.Fatalf("parse %s = %v, want a policy error", tt.raw, err)
				}
				return
			}
			if err := checkFetchURL(target); !errors.Is(err, tt.cause) {
				t.Fatalf("checkFetchURL(%s) = %v, want %v", tt.raw, err, tt.cause)
			}
		})
	}
}

func TestFetchReadsMainArticleNotNavigationOrScript(t *testing.T) {
	body := strings.Repeat("City won the match with a late goal. The crowd stayed to applaud the team. ", 12)
	page := `<!doctype html><html><head><title>City beat United - Daily Test</title>
<style>.nav{color:red}</style>
<script>var secretScript = "SCRIPT_MARKER";</script></head>
<body><nav>NAV_MARKER Home World</nav>
<main><article><h1>City beat United</h1>
<p>` + body + `</p>
<p>Second paragraph stays readable.</p></article></main>
<footer>FOOTER_MARKER</footer></body></html>`
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second), MaxChars: 4000},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		}), map[string][]netip.Addr{"article.example.test": publicV4()})

	out, err := fixture.fetcher.Fetch(context.Background(), "http://article.example.test/story")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.URL != "http://article.example.test/story" {
		t.Fatalf("URL = %q", out.URL)
	}
	if !strings.Contains(out.Title, "City beat United") {
		t.Fatalf("title = %q, want the article title", out.Title)
	}
	if !strings.Contains(out.Text, "City won the match with a late goal") {
		t.Fatalf("text does not contain the main article: %q", out.Text)
	}
	if strings.Contains(out.Text, "NAV_MARKER") {
		t.Fatalf("navigation leaked into the article: %q", out.Text)
	}
	if strings.Contains(out.Text, "SCRIPT_MARKER") || strings.Contains(out.Text, "color:red") {
		t.Fatalf("script or style leaked into the article: %q", out.Text)
	}
	if out.Truncated {
		t.Fatal("a short article was marked truncated")
	}
	targets := fixture.dialer.recorded()
	if len(targets) != 1 || targets[0] != "93.184.216.34:80" {
		t.Fatalf("dial targets = %v, want the vetted public literal", targets)
	}
}

func TestFetchTruncatesRunesAndMarksTheCut(t *testing.T) {
	// A multibyte body makes a byte-based cut visible as invalid UTF-8.
	body := strings.Repeat("東京で試合が行われ、市民がスタジアムに集まった。⚽ ", 40)
	page := `<html><head><title>東京の試合</title></head><body><article><h1>東京の試合</h1><p>` + body + `</p></article></body></html>`
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second), MaxChars: 120},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		}), map[string][]netip.Addr{"article.example.test": publicV4()})

	out, err := fixture.fetcher.Fetch(context.Background(), "http://article.example.test/story")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !out.Truncated {
		t.Fatal("long article was not marked truncated")
	}
	if !utf8.ValidString(out.Text) {
		t.Fatalf("truncated text is not valid UTF-8: %q", out.Text)
	}
	if got := len([]rune(out.Text)); got > 120 {
		t.Fatalf("truncated text has %d runes, want at most 120", got)
	}
	if !strings.HasSuffix(out.Text, "…") {
		t.Fatalf("truncated text does not mark the cut: %q", out.Text)
	}
}

func TestFetchPinsTheVettedAddressAgainstDNSRebinding(t *testing.T) {
	page := `<html><head><title>Pinned</title></head><body><article><p>` +
		strings.Repeat("The pinned article body is long enough for readability. ", 20) +
		`</p></article></body></html>`
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		}), map[string][]netip.Addr{"rebind.example.test": publicV4()})
	// The second answer is what an unguarded second lookup would see.
	fixture.resolver.sequences["rebind.example.test"] = [][]netip.Addr{
		{netip.MustParseAddr("93.184.216.34")},
		{netip.MustParseAddr("127.0.0.1")},
	}

	if _, err := fixture.fetcher.Fetch(context.Background(), "http://rebind.example.test/story"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls := fixture.resolver.callCount("rebind.example.test"); calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly the vetted lookup", calls)
	}
	targets := fixture.dialer.recorded()
	if len(targets) != 1 || targets[0] != "93.184.216.34:80" {
		t.Fatalf("dial targets = %v, want the first, vetted literal", targets)
	}
}

func TestFetchRejectsMixedPublicAndPrivateDNS(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("handler ran for a mixed DNS answer")
		}), map[string][]netip.Addr{"mixed.example.test": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("10.0.0.5"),
		}})

	_, err := fixture.fetcher.Fetch(context.Background(), "http://mixed.example.test/story")
	if !errors.Is(err, errBlockedAddress) {
		t.Fatalf("Fetch error = %v, want a blocked address", err)
	}
	if got := fetchFailureMessage(err); got != fetchBlocked {
		t.Fatalf("failure message = %q", got)
	}
	if targets := fixture.dialer.recorded(); len(targets) != 0 {
		t.Fatalf("dial targets = %v, want no dial", targets)
	}
}

func TestFetchReusesThePinAcrossSameHostRedirects(t *testing.T) {
	page := `<html><head><title>Redirect pin</title></head><body><article><p>` +
		strings.Repeat("The same-host redirect keeps the first vetted address. ", 20) +
		`</p></article></body></html>`
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/story", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		}), map[string][]netip.Addr{"article.example.test": publicV4()})
	fixture.resolver.sequences["article.example.test"] = [][]netip.Addr{
		{netip.MustParseAddr("93.184.216.34")},
		{netip.MustParseAddr("127.0.0.1")},
	}

	out, err := fixture.fetcher.Fetch(context.Background(), "http://article.example.test/start")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out.URL != "http://article.example.test/story" {
		t.Fatalf("final URL = %q", out.URL)
	}
	if calls := fixture.resolver.callCount("article.example.test"); calls != 1 {
		t.Fatalf("resolver calls = %d, want the redirect to reuse the pin", calls)
	}
	targets := fixture.dialer.recorded()
	if len(targets) != 1 || targets[0] != "93.184.216.34:80" {
		t.Fatalf("dial targets = %v, want only the vetted literal", targets)
	}
}

func TestFetchRejectsHTTPSDowngrade(t *testing.T) {
	fixture := newFetchFixture(t, DefaultFetchConfig(),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		map[string][]netip.Addr{"article.example.test": publicV4()})
	pins := pinSet{"article.example.test": publicV4()}
	previous := &http.Request{URL: mustURL(t, "https://article.example.test/start")}
	next := (&http.Request{URL: mustURL(t, "http://article.example.test/next")}).
		WithContext(context.WithValue(context.Background(), pinKey{}, pins))

	err := fixture.fetcher.checkRedirect(next, []*http.Request{previous})
	if !errors.Is(err, errBlockedAddress) {
		t.Fatalf("checkRedirect error = %v, want an https-to-http refusal", err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return parsed
}

func TestFetchRevalidatesEveryRedirect(t *testing.T) {
	page := `<html><head><title>Redirected</title></head><body><article><p>` +
		strings.Repeat("The redirect target is a real article with enough text. ", 20) +
		`</p></article></body></html>`
	tests := []struct {
		name     string
		location string
		answers  map[string][]netip.Addr
	}{
		{
			name:     "literal loopback",
			location: "http://127.0.0.1/secret",
		},
		{
			name:     "literal tailnet",
			location: "http://100.100.100.100/secret",
		},
		{
			name:     "literal ula tailnet",
			location: "http://[fd7a:115c:a1e0::1]/secret",
		},
		{
			name:     "host resolving private",
			location: "http://private.example.test/secret",
			answers:  map[string][]netip.Addr{"private.example.test": {netip.MustParseAddr("192.168.1.20")}},
		},
		{
			name:     "host rebinding to loopback",
			location: "http://rebind.example.test/secret",
			answers:  map[string][]netip.Addr{"rebind.example.test": {netip.MustParseAddr("127.0.0.1")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answers := map[string][]netip.Addr{"article.example.test": publicV4()}
			for host, addrs := range tt.answers {
				answers[host] = addrs
			}
			fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/start" {
						http.Redirect(w, r, tt.location, http.StatusFound)
						return
					}
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					fmt.Fprint(w, page)
				}), answers)

			_, err := fixture.fetcher.Fetch(context.Background(), "http://article.example.test/start")
			if !errors.Is(err, errBlockedAddress) {
				t.Fatalf("Fetch error = %v, want a blocked redirect", err)
			}
			if got := fetchFailureMessage(err); got != fetchBlocked {
				t.Fatalf("failure message = %q", got)
			}
			for _, target := range fixture.dialer.recorded() {
				host, _, splitErr := net.SplitHostPort(target)
				if splitErr == nil {
					addr, parseErr := netip.ParseAddr(host)
					if parseErr == nil && !publicAddress(addr) {
						t.Fatalf("dial target %q is private; the redirect guard ran too late", target)
					}
				}
			}
		})
	}
}

func TestFetchBoundsRedirects(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second), MaxRedirects: 2},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", len(r.URL.Path)), http.StatusFound)
		}), map[string][]netip.Addr{"article.example.test": publicV4()})

	_, err := fixture.fetcher.Fetch(context.Background(), "http://article.example.test/hop/0")
	if !errors.Is(err, errTooManyRedirects) {
		t.Fatalf("Fetch error = %v, want the redirect bound", err)
	}
	if got := fetchFailureMessage(err); got != fetchUnavailable {
		t.Fatalf("failure message = %q", got)
	}
}

func TestFetchRejectsNonHTML(t *testing.T) {
	for _, contentType := range []string{
		"application/pdf",
		"image/png",
		"text/plain; charset=utf-8",
		"application/json",
		"application/octet-stream",
	} {
		t.Run(contentType, func(t *testing.T) {
			fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", contentType)
					fmt.Fprint(w, `<html><body><p>not really html</p></body></html>`)
				}), map[string][]netip.Addr{"files.example.test": publicV4()})

			_, err := fixture.fetcher.Fetch(context.Background(), "http://files.example.test/document")
			if !errors.Is(err, errFetchNotHTML) {
				t.Fatalf("Fetch error = %v, want a non-HTML error", err)
			}
			if got := fetchFailureMessage(err); got != fetchNotHTML {
				t.Fatalf("failure message = %q", got)
			}
		})
	}
}

func TestFetchRejectsOversizeBodies(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "streamed body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, strings.Repeat("x", 4096))
			},
		},
		{
			name: "declared content length",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Content-Length", "4096")
				fmt.Fprint(w, "<html></html>")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second), MaxBytes: 1024},
				tt.handler, map[string][]netip.Addr{"big.example.test": publicV4()})
			_, err := fixture.fetcher.Fetch(context.Background(), "http://big.example.test/page")
			if !errors.Is(err, errFetchTooLarge) {
				t.Fatalf("Fetch error = %v, want too large", err)
			}
			if got := fetchFailureMessage(err); got != fetchTooLarge {
				t.Fatalf("failure message = %q", got)
			}
		})
	}
}

func TestFetchTimesOutCleanly(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(30 * time.Millisecond)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, "<html></html>")
		}), map[string][]netip.Addr{"slow.example.test": publicV4()})

	_, err := fixture.fetcher.Fetch(context.Background(), "http://slow.example.test/page")
	if err == nil {
		t.Fatal("Fetch succeeded against a slow page, want a timeout")
	}
	if got := fetchFailureMessage(err); got != fetchUnavailable {
		t.Fatalf("failure message = %q", got)
	}
	// The fixed line is what reaches the model; the raw timeout detail may
	// name the host locally but never becomes the Tool error.
	if strings.Contains(fetchFailureMessage(err), "http://") {
		t.Fatalf("failure message leaked the URL: %q", fetchFailureMessage(err))
	}
}

func TestFetchReportsAnUnreadablePage(t *testing.T) {
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><head><title>Menu</title></head><body><script>var x = 1;</script></body></html>`)
		}), map[string][]netip.Addr{"empty.example.test": publicV4()})

	_, err := fixture.fetcher.Fetch(context.Background(), "http://empty.example.test/page")
	if !errors.Is(err, errFetchUnreadable) {
		t.Fatalf("Fetch error = %v, want unreadable", err)
	}
	if got := fetchFailureMessage(err); got != fetchNoArticle {
		t.Fatalf("failure message = %q", got)
	}
}

func TestFetchDialsPinnedDualStackLiterals(t *testing.T) {
	page := `<html><head><title>Dual stack</title></head><body><article><p>` +
		strings.Repeat("Dual stack content is enough for the reader to parse it. ", 20) +
		`</p></article></body></html>`
	v6 := netip.MustParseAddr("2606:2800:220:1:248:1893:25c8:1946")
	v4 := netip.MustParseAddr("93.184.216.35")
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		}), map[string][]netip.Addr{"dual.example.test": {v6, v4}})
	// Fail the first family so the fetcher has to try the second pinned IP.
	fixture.dialer.fail = func(address string) bool {
		return strings.HasPrefix(address, "[2606:2800:")
	}

	if _, err := fixture.fetcher.Fetch(context.Background(), "http://dual.example.test/story"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := []string{"[2606:2800:220:1:248:1893:25c8:1946]:80", "93.184.216.35:80"}
	got := fixture.dialer.recorded()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dial targets = %v, want %v", got, want)
	}
}

func TestFetchConcurrentRequestsKeepTheirOwnPin(t *testing.T) {
	page := `<html><head><title>Concurrent</title></head><body><article><p>` +
		strings.Repeat("Concurrent requests each keep their own vetted address. ", 20) +
		`</p></article></body></html>`
	fixture := newFetchFixture(t, FetchConfig{Timeout: config.Duration(2 * time.Second)},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(5 * time.Millisecond)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
		}), nil)

	const requests = 8
	for i := 0; i < requests; i++ {
		host := fmt.Sprintf("host%d.example.test", i)
		fixture.resolver.answers[host] = []netip.Addr{netip.MustParseAddr(fmt.Sprintf("93.184.216.%d", 100+i))}
	}
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := fmt.Sprintf("host%d.example.test", i)
			out, err := fixture.fetcher.Fetch(context.Background(), "http://"+host+"/story")
			if err != nil {
				errs <- fmt.Errorf("%s: %w", host, err)
				return
			}
			if out.URL != "http://"+host+"/story" {
				errs <- fmt.Errorf("%s: final URL = %q", host, out.URL)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(fixture.dialer.recorded()); got != requests {
		t.Fatalf("dial count = %d, want %d", got, requests)
	}
}
