package httpclient

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// ErrInsecureOrigin reports a credential-bearing upstream origin that would
// send its secret over plain HTTP to a host that is not this machine.
var ErrInsecureOrigin = errors.New("plain http would send the credential in clear text")

// RequireSecureOrigin refuses a credential-bearing upstream origin that would
// travel in clear text. HTTPS is always accepted; plain HTTP only for a loopback
// host (localhost, 127.0.0.0/8 or ::1) or when the caller's operator config
// deliberately opts in. A base path such as /api/v3 is preserved, so a custom
// GitHub Enterprise origin keeps working. The error never echoes the URL: an
// operator value can carry userinfo.
func RequireSecureOrigin(raw string, allowInsecure bool) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("must be a valid http or https URL with a host")
	}
	if parsed.Scheme == "https" || allowInsecure || loopbackHost(parsed.Hostname()) {
		return nil
	}
	return ErrInsecureOrigin
}

// InsecureOrigin reports whether a well-formed origin would send a credential
// over plain HTTP to a non-loopback host: the one case an opt-in turns from an
// unnoticed default into a deliberate, logged choice. A malformed URL reports
// false; RequireSecureOrigin refuses it separately.
func InsecureOrigin(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Scheme == "http" && parsed.Host != "" && !loopbackHost(parsed.Hostname())
}

// loopbackHost is the same rule the inbound listeners use: "localhost" and any
// 127.0.0.0/8 or ::1 literal names this machine, so a local fake can keep
// using plain HTTP in its fixtures.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
