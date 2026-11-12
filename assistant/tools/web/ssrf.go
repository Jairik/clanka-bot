package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The fetch Tool's address policy. web.search deliberately targets a private
// Compose hostname, so its client cannot be reused for arbitrary URLs: this
// guard is the difference between "read a public article" and "let the model
// reach the Pi's services or the tailnet".
var (
	errBlockedAddress   = errors.New("address is not public")
	errBadFetchURL      = errors.New("not a full http or https URL")
	errCredentials      = errors.New("URL carries credentials")
	errNoAddress        = errors.New("host has no address")
	errTooManyRedirects = errors.New("too many redirects")
)

// blockedPrefixes are the special-purpose ranges that are not public unicast.
// netip's predicates cover loopback, private, link-local and multicast first;
// these add the ranges Go still calls global unicast: CGNAT (Tailscale),
// documentation, benchmarking, NAT64, 6to4 and the deprecated site-local block.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT and Tailscale 100.x addresses
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),           // unspecified, loopback, IPv4-compatible
	netip.MustParsePrefix("::ffff:0:0/96"),   // IPv4-mapped (Unmap handles it too)
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated (not covered by Unmap)
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 local-use prefix
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments, incl. Teredo
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 embeds an IPv4 address
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local
}

// publicAddress reports whether addr may be dialled for a public page fetch.
// The check runs on every DNS answer and again on every dial target, so a
// resolver that changes its answer between the two cannot slip a private
// address through.
func publicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be judged as its IPv4 self.
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() || !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// resolver is the DNS seam. net.DefaultResolver implements it; tests inject a
// scripted answer so no test ever contacts a real private address.
type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// pinKey carries one request's vetted addresses to its dialer. A value per
// request keeps concurrent fetches from sharing or overwriting each other's
// DNS answer, which is what makes the pin a real rebinding defence.
type pinKey struct{}

// pinSet maps a normalised hostname to the addresses validated for it.
type pinSet map[string][]netip.Addr

// normalizeHost lowercases a hostname and drops the DNS root dot, so a pin
// written for "Example.COM." matches the dialer's "example.com".
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// checkFetchURL validates everything decidable without DNS: the scheme, the
// absence of credentials, and a literal host that is already private. It is
// also the check every redirect repeats before the request leaves the process.
func checkFetchURL(target *url.URL) error {
	if target == nil {
		return errBadFetchURL
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", errBadFetchURL, target.Scheme)
	}
	if target.Opaque != "" || target.Host == "" {
		return errBadFetchURL
	}
	// A URL with user:password@ in it is a credential, not a public page. It
	// must never be forwarded to an upstream or into a log line.
	if target.User != nil {
		return errCredentials
	}
	host := normalizeHost(target.Hostname())
	if host == "" {
		return errBadFetchURL
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%w: localhost", errBlockedAddress)
	}
	if strings.Contains(host, "%") {
		return errBadFetchURL
	}
	if port := target.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("%w: port %q", errBadFetchURL, port)
		}
	}
	// A literal IP is checked before DNS. IPv6 forms arrive without brackets
	// from URL.Hostname(); netip handles IPv4-mapped and zoned forms.
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddress(addr) {
			return fmt.Errorf("%w: %s", errBlockedAddress, addr)
		}
		return nil
	}
	// Legacy numeric IPv4 spellings (2130706433, 0x7f000001, 127.1) are not
	// DNS names. Rejecting them closes the resolver-dependent gap where libc
	// would interpret them as loopback.
	if numericHost(host) {
		return fmt.Errorf("%w: numeric host %q", errBlockedAddress, host)
	}
	return nil
}

// numericHost reports whether the host is a legacy numeric IPv4 spelling:
// every rune is a digit, a hex letter or the punctuation those forms use, and
// at least one digit is present. A real DNS name has a non-hex letter or a
// hyphen (and its last label is never all-numeric), so this does not reject an
// ordinary public hostname such as dead.cab.
func numericHost(host string) bool {
	if host == "" {
		return false
	}
	hasDigit := false
	for _, r := range host {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '.' || r == 'x' || r == 'X':
		default:
			return false
		}
	}
	return hasDigit
}

// resolveHost resolves one hostname once and validates every answer. A mixed
// public/private answer fails closed: fetching only the public member would
// leave the private one available to a later connection.
func (f *fetcher) resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	host = normalizeHost(host)
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddress(addr) {
			return nil, fmt.Errorf("%w: %s", errBlockedAddress, addr)
		}
		return []netip.Addr{addr}, nil
	}
	addrs, err := f.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s", errNoAddress, host)
	}
	for _, addr := range addrs {
		if !publicAddress(addr) {
			return nil, fmt.Errorf("%w: %s resolves to %s", errBlockedAddress, host, addr.Unmap())
		}
	}
	return addrs, nil
}

// prepare validates the URL, resolves it once and returns the pinned set the
// dialer must use. No second lookup happens between this point and the dial.
func (f *fetcher) prepare(ctx context.Context, rawURL string) (*url.URL, pinSet, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, nil, errBadFetchURL
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errBadFetchURL, err)
	}
	if err := checkFetchURL(target); err != nil {
		return nil, nil, err
	}
	host := normalizeHost(target.Hostname())
	addrs, err := f.resolveHost(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	return target, pinSet{host: addrs}, nil
}

// checkRedirect revalidates every hop. A redirect to a different host is
// resolved and pinned afresh; a same-host redirect keeps its existing pin, so
// a redirect cannot trigger a second DNS answer for the host already vetted.
func (f *fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.maxRedirects {
		return errTooManyRedirects
	}
	if err := checkFetchURL(req.URL); err != nil {
		return err
	}
	pins, _ := req.Context().Value(pinKey{}).(pinSet)
	if pins == nil {
		pins = pinSet{}
	}
	host := normalizeHost(req.URL.Hostname())
	if _, ok := pins[host]; !ok {
		addrs, err := f.resolveHost(req.Context(), host)
		if err != nil {
			return err
		}
		pins[host] = addrs
	}
	*req = *req.WithContext(context.WithValue(req.Context(), pinKey{}, pins))
	return nil
}

// dialContext dials only the literal IPs vetted for this request. The Host
// header and TLS SNI still name the original hostname, but the connection
// itself cannot be redirected by a second DNS answer.
func (f *fetcher) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial address %q: %w", address, err)
	}
	host = normalizeHost(host)
	pins, _ := ctx.Value(pinKey{}).(pinSet)
	addrs, pinned := pins[host]
	if !pinned || len(addrs) == 0 {
		// Defensive fallback for a dial that did not come from prepare (for
		// example a caller that reused the transport directly). It still
		// validates before dialling.
		addrs, err = f.resolveHost(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	var lastErr error
	for _, addr := range addrs {
		if !publicAddress(addr) {
			return nil, fmt.Errorf("%w: %s", errBlockedAddress, addr.Unmap())
		}
		literal := net.JoinHostPort(addr.Unmap().String(), port)
		conn, dialErr := f.dial(ctx, network, literal)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errNoAddress
	}
	return nil, lastErr
}

// fetchDialTimeout is the dialer's own ceiling. The request context and client
// timeout are the outer bounds; this keeps a black-holed TCP connect from
// outliving either.
const fetchDialTimeout = 2 * time.Second
