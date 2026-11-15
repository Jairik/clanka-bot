package web

import (
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// provenanceWindow is how long a URL returned by web.search stays
	// fetchable. The documented "read me that article" flow may follow a
	// search from an earlier turn in the same conversation; a URL an injected
	// page invents was never returned by a search.
	provenanceWindow = 30 * time.Minute
	// provenanceMaxEntries bounds the cache. A response returns at most
	// maxResultLimit URLs, so this holds many recent searches without growth.
	provenanceMaxEntries = 256
)

// Provenance remembers the URLs web.search returned recently, so web.fetch can
// accept a search result (even in a later turn) and refuse an address that only
// an untrusted page invented. It is shared by the module's two Tools and is
// safe for concurrent use. It does not replace the SSRF policy; it adds to it.
type Provenance struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	now    func() time.Time
	seen   map[string]time.Time
	// order is insertion order, so eviction at the bound is deterministic
	// even when one search page records every URL at the same instant.
	order []string
}

// NewProvenance builds the shipped cache: a 30-minute exact-match window with a
// 256-entry bound.
func NewProvenance() *Provenance {
	return newProvenance(provenanceWindow, provenanceMaxEntries, time.Now)
}

// newProvenance is the constructor seam. Tests pass a fake clock, a short
// window or a small bound so expiry and eviction are deterministic.
func newProvenance(window time.Duration, max int, now func() time.Time) *Provenance {
	if window <= 0 {
		window = provenanceWindow
	}
	if max <= 0 {
		max = provenanceMaxEntries
	}
	if now == nil {
		now = time.Now
	}
	return &Provenance{
		window: window,
		max:    max,
		now:    now,
		seen:   make(map[string]time.Time),
	}
}

// Record notes the URLs one web.search response handed to the model. A URL
// that is recorded again refreshes its window rather than adding a second
// entry. An unusable link is skipped; the search provider already filtered it.
func (p *Provenance) Record(urls []string) {
	if p == nil || len(urls) == 0 {
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, raw := range urls {
		key, ok := provenanceKey(raw)
		if !ok {
			continue
		}
		if _, exists := p.seen[key]; !exists {
			p.pruneLocked(now)
			p.order = append(p.order, key)
		}
		p.seen[key] = now
	}
}

// Allows reports whether web.fetch may open rawURL: it was returned by a
// recent search, or the user named it in this turn's utterance. The check is
// exact after normalisation, so a page cannot mint a fresh URL that passes.
func (p *Provenance) Allows(rawURL, userText string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if p == nil || rawURL == "" {
		return false
	}
	if userMentions(userText, rawURL) {
		return true
	}
	key, ok := provenanceKey(rawURL)
	if !ok {
		return false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	seenAt, ok := p.seen[key]
	if !ok {
		return false
	}
	if now.Sub(seenAt) > p.window {
		delete(p.seen, key)
		return false
	}
	return true
}

// pruneLocked drops expired entries, then the oldest-inserted ones until a
// new entry fits the bound. It runs under the cache mutex.
func (p *Provenance) pruneLocked(now time.Time) {
	kept := p.order[:0]
	for _, key := range p.order {
		seenAt, ok := p.seen[key]
		if !ok {
			continue
		}
		if now.Sub(seenAt) > p.window {
			delete(p.seen, key)
			continue
		}
		kept = append(kept, key)
	}
	p.order = kept
	for len(p.seen) >= p.max && len(p.order) > 0 {
		delete(p.seen, p.order[0])
		p.order = p.order[1:]
	}
}

// userMentions reports whether the turn's utterance contains the URL verbatim,
// matching either the model's spelling or the normalised form.
func userMentions(userText, rawURL string) bool {
	if userText == "" {
		return false
	}
	if strings.Contains(userText, rawURL) {
		return true
	}
	key, ok := provenanceKey(rawURL)
	return ok && strings.Contains(userText, key)
}

// provenanceKey normalises a URL to the scheme, host, path and query that
// identify the resource. Case-folded scheme and host, a dropped fragment and a
// dropped default port are the only equivalences: the path, query and any
// non-default port have to match exactly, so an injected variant cannot reuse
// a vetted URL.
func provenanceKey(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := normalizeHost(parsed.Hostname())
	if host == "" {
		return "", false
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	key := scheme + "://" + hostPort + path
	if parsed.RawQuery != "" {
		key += "?" + parsed.RawQuery
	}
	return key, true
}
