// Package httpguard holds the browser-facing request checks shared by the
// localhost debug listener and the authenticated Dashboard: a Host allowlist
// that stops DNS rebinding, the same-origin CSRF check, and the JSON
// Content-Type requirement for JSON mutation bodies. Both surfaces used to
// carry their own copy; one policy keeps them from drifting apart.
package httpguard

import (
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Hosts is an immutable Host-header allowlist. Its zero value accepts the
// documented loopback names only, which is the safe default for a local
// listener. Extra names must be exact public hostnames; wildcards are refused.
type Hosts struct {
	names map[string]bool
}

// NewHosts validates and freezes the extra public hostnames. Each entry is a
// bare hostname with an optional port, never a URL or a pattern. The loopback
// names (localhost, 127.0.0.1, [::1]) are always accepted because the local
// smoke test and Docker healthcheck use them.
func NewHosts(entries []string) (Hosts, error) {
	hosts := Hosts{names: make(map[string]bool, len(entries))}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		name, err := normalizeHost(entry)
		if err != nil {
			return Hosts{}, err
		}
		hosts.names[name] = true
	}
	return hosts, nil
}

// normalizeHost validates one configured hostname. A bare host or host:port is
// accepted; a scheme, path, credentials, wildcard or whitespace is not.
func normalizeHost(entry string) (string, error) {
	if strings.ContainsAny(entry, " \t\r\n") {
		return "", fmt.Errorf("httpguard: host %q contains whitespace", entry)
	}
	if strings.Contains(entry, "://") || strings.ContainsAny(entry, "/?#@*") {
		return "", fmt.Errorf("httpguard: host %q must be a bare hostname or host:port, not a pattern or URL", entry)
	}
	if name, _, err := net.SplitHostPort(entry); err == nil {
		if name == "" {
			return "", fmt.Errorf("httpguard: host %q has no name", entry)
		}
		return strings.ToLower(strings.Trim(name, "[]")), nil
	}
	return strings.ToLower(strings.Trim(entry, "[]")), nil
}

// Allows reports whether r's Host header is in the allowlist. A missing Host,
// a malformed one, or an unlisted name is refused, so a rebound page cannot
// borrow a loopback source address.
func (h Hosts) Allows(r *http.Request) bool {
	if r == nil {
		return false
	}
	name, ok := RequestHost(r)
	if !ok {
		return false
	}
	if IsLoopbackHost(name) {
		return true
	}
	return h.names[strings.ToLower(name)]
}

// RequestHost extracts the hostname from the Host header, dropping any port.
func RequestHost(r *http.Request) (string, bool) {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return "", false
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", false
	}
	return host, true
}

// IsLoopbackHost reports whether a hostname names this machine's loopback
// interface. "localhost" is the documented curl smoke name; the literal
// addresses cover curl, Docker healthchecks and the Dashboard page.
func IsLoopbackHost(name string) bool {
	switch strings.ToLower(strings.Trim(name, "[]")) {
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(name, "[]"))
	return ip != nil && ip.IsLoopback()
}

// SameOrigin is the strict CSRF guard used by the authenticated Dashboard. A
// mutation needs browser Fetch Metadata saying same-origin, or an Origin (or
// Referer) naming this Host, including a proxy-forwarded Host. A request with
// no origin at all is refused; the Dashboard is a browser surface.
func SameOrigin(r *http.Request) bool {
	if r == nil || isSafeMethod(r.Method) {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
		// Older clients do not send Fetch Metadata; fall through to Origin.
	default:
		return false
	}
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return false
	}
	return namesThisHost(r, source)
}

// SameOriginOrLocal is SameOrigin with one exception: a mutating request with
// no browser origin metadata at all is accepted. That is what a local curl or
// script looks like, and the documented debug smoke test uses it. The Host
// allowlist and the debug source check remain the barrier. Any browser-supplied
// Origin, Referer or Fetch Metadata is still checked, so a cross-site simple
// request is refused.
func SameOriginOrLocal(r *http.Request) bool {
	if r == nil || isSafeMethod(r.Method) {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
	default:
		return false
	}
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		// A non-browser client. Host and source checks still apply.
		return true
	}
	return namesThisHost(r, source)
}

// namesThisHost reports whether source names the request's Host, either
// directly or through the trusted local proxy's X-Forwarded-Host.
func namesThisHost(r *http.Request, source string) bool {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	if strings.EqualFold(parsed.Host, r.Host) {
		return true
	}
	forwarded := r.Header.Get("X-Forwarded-Host")
	if forwarded == "" {
		return false
	}
	// X-Forwarded-Host can be a comma-separated chain; the first entry is the
	// original host set by the trusted local proxy.
	first, _, _ := strings.Cut(forwarded, ",")
	return strings.EqualFold(parsed.Host, strings.TrimSpace(first))
}

// IsJSON reports whether a mutation body declares application/json. Parameters
// such as charset are allowed. JSON endpoints require this so a cross-site
// text/plain form post, which needs no CORS preflight, cannot reach a handler
// that decodes JSON.
func IsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

// NeedsJSONBody reports whether this method normally carries a JSON mutation
// body. DELETE is excluded because a settings delete has no body.
func NeedsJSONBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
