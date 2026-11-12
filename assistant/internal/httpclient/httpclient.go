// Package httpclient builds the keep-alive HTTP clients Tool modules use for
// their upstreams. One client per upstream, reused across requests, so a
// burst of Agent Tool calls does not pay a new TCP and TLS handshake each
// time.
package httpclient

import (
	"net"
	"net/http"
	"time"
)

// Options tunes one upstream client. Zero values use the defaults below.
type Options struct {
	// Timeout bounds one request including reading the body. It is the
	// outer safety net; callers still pass a context deadline.
	Timeout time.Duration
	// MaxIdleConns and MaxIdleConnsPerHost keep warm connections for the
	// next call. A small pool is enough for a voice assistant's Tool calls.
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	// IdleConnTimeout closes a connection the upstream has already closed.
	IdleConnTimeout time.Duration
	// DialTimeout, TLSHandshakeTimeout and ResponseHeaderTimeout bound the
	// phases a black-holed upstream can stretch indefinitely.
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

const (
	defaultTimeout             = 1500 * time.Millisecond
	defaultMaxIdleConns        = 8
	defaultMaxIdleConnsPerHost = 4
	defaultIdleConnTimeout     = 90 * time.Second
	defaultDialTimeout         = 2 * time.Second
	defaultTLSHandshakeTimeout = 2 * time.Second
)

// New returns a keep-alive client. The returned client is safe for concurrent
// use and should be built once per upstream, not per request.
func New(opts Options) *http.Client {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = defaultMaxIdleConns
	}
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = defaultIdleConnTimeout
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = defaultDialTimeout
	}
	if opts.TLSHandshakeTimeout <= 0 {
		opts.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	}
	if opts.ResponseHeaderTimeout <= 0 {
		// The caller's request timeout is the right header budget: a provider
		// configured for a longer call must not be cut off by a shorter,
		// unrelated default.
		opts.ResponseHeaderTimeout = opts.Timeout
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   opts.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          opts.MaxIdleConns,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport, Timeout: opts.Timeout}
}
