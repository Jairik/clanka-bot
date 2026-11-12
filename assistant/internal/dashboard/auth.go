package dashboard

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// identityHeader is the only header trusted for authentication. Serve adds it
// from the authenticated tailnet identity and strips client-supplied copies.
const identityHeader = "Tailscale-User-Login"

type loginKey struct{}

// Login returns the validated Tailscale login for a served request.
func Login(ctx context.Context) string {
	login, _ := ctx.Value(loginKey{}).(string)
	return login
}

// identity reads exactly one non-empty identity header. Duplicates are
// ambiguous and rejected, as are requests with no header at all (tagged
// devices and Funnel traffic never receive identity headers).
func (h *Handler) identity(r *http.Request) (string, bool) {
	values := r.Header.Values(identityHeader)
	if len(values) != 1 {
		return "", false
	}
	login := strings.TrimSpace(values[0])
	if login == "" {
		return "", false
	}
	return login, true
}

// stripTailscaleHeaders removes every Tailscale-supplied header after auth so
// a handler cannot accidentally trust an unvalidated identity or capability
// header. The validated login travels in the request context instead.
func stripTailscaleHeaders(header http.Header) {
	for name := range header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Tailscale-") {
			delete(header, name)
		}
	}
}

// sourceAllowed trusts only loopback and the container's default gateway (the
// host that publishes the loopback port). A direct remote connection is
// refused even if it carries a plausible identity header, so exposing the
// listener by mistake does not turn client headers into authentication.
func (h *Handler) sourceAllowed(remoteAddr string) bool {
	host := remoteAddr
	if parsed, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = parsed
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	if h.proxyAllowed != nil {
		return h.proxyAllowed(ip)
	}
	return ip.IsLoopback() || ip.Equal(containerGateway())
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request, reason, login string) {
	// Log the attempt for audit, but bound the caller-controlled value so a
	// huge header cannot flood the logs. The response body is deliberately
	// identical for missing, duplicate and wrong identities.
	if len(login) > 128 {
		login = login[:128]
	}
	h.logger.Warn("dashboard auth rejected",
		"reason", reason,
		"path", r.URL.Path,
		"remote", r.RemoteAddr,
		"login", login,
	)
	http.Error(w, "forbidden", http.StatusForbidden)
}

// SameOrigin is the CSRF guard for mutating routes. Safe methods pass. For a
// mutation, a browser-supplied same-origin fetch marker is accepted, otherwise
// Origin (or Referer) must name this host, including a proxy-forwarded host.
func SameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
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

var (
	gatewayOnce sync.Once
	gatewayIP   net.IP
)

// containerGateway reads this container's default gateway once. Compose
// publishes the Dashboard on host loopback, and Docker's proxy reaches the
// container from that gateway; sibling containers arrive from their own
// addresses and are refused.
func containerGateway() net.IP {
	gatewayOnce.Do(func() {
		raw, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[1] != "00000000" {
				continue
			}
			// The Gateway column is little-endian hex.
			value, err := strconv.ParseUint(fields[2], 16, 32)
			if err != nil {
				continue
			}
			gatewayIP = net.IPv4(byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
			return
		}
	})
	return gatewayIP
}
