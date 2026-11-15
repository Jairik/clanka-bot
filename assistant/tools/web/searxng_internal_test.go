package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// redactionMarker stands in for Note or List text a search query may carry. It
// contains spaces so the encoded q= value differs from the raw text.
const redactionMarker = "private note marker 7f3a"

// assertNoQuery fails if text carries the raw or URL-encoded query marker.
func assertNoQuery(t *testing.T, text string) {
	t.Helper()
	for _, leaked := range []string{
		redactionMarker,
		url.QueryEscape(redactionMarker),
		strings.ReplaceAll(redactionMarker, " ", "+"),
	} {
		if strings.Contains(text, leaked) {
			t.Fatalf("query text %q leaked into %q", leaked, text)
		}
	}
	if strings.Contains(text, "q=") {
		t.Fatalf("query parameter leaked into %q", text)
	}
}

// roundTripFunc lets one test inject a transport failure without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestSearchTransportErrorIsBounded exercises the real client.Do path: the
// returned *url.Error repeats the full request URL, including q=. The Provider
// replaces that with a kind and the host, while errors.Is still sees a timeout.
func TestSearchTransportErrorIsBounded(t *testing.T) {
	t.Run("connect", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		baseURL := server.URL
		server.Close()

		provider := newSearxng(baseURL, 200*time.Millisecond, nil)
		_, err := provider.Search(context.Background(), redactionMarker, 5)
		if err == nil {
			t.Fatal("search against a closed server succeeded")
		}
		assertNoQuery(t, err.Error())
		if got := failureKind(err); got != "connect" {
			t.Fatalf("kind = %q, want connect: %v", got, err)
		}
		if !strings.Contains(err.Error(), endpointHost(baseURL)) {
			t.Fatalf("error %q does not name the upstream host", err)
		}
	})

	t.Run("dns", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.DNSError{Err: "no such host", Name: "web-search"}
		})}
		provider := newSearxng("http://web-search:8080", time.Second, client)
		_, err := provider.Search(context.Background(), redactionMarker, 5)
		if err == nil {
			t.Fatal("search with a failing resolver succeeded")
		}
		assertNoQuery(t, err.Error())
		if got := failureKind(err); got != "dns" {
			t.Fatalf("kind = %q, want dns: %v", got, err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprint(w, `{"results":[]}`)
		}))
		defer server.Close()

		provider := newSearxng(server.URL, 20*time.Millisecond, nil)
		_, err := provider.Search(context.Background(), redactionMarker, 5)
		if err == nil {
			t.Fatal("search against a slow server succeeded")
		}
		assertNoQuery(t, err.Error())
		if got := failureKind(err); got != "timeout" {
			t.Fatalf("kind = %q, want timeout: %v", got, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout is no longer identifiable with errors.Is: %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		provider := newSearxng("http://web-search:8080", time.Second, nil)
		_, err := provider.Search(ctx, redactionMarker, 5)
		if err == nil {
			t.Fatal("search with a cancelled context succeeded")
		}
		assertNoQuery(t, err.Error())
		if got := failureKind(err); got != "cancelled" {
			t.Fatalf("kind = %q, want cancelled: %v", got, err)
		}
	})

	t.Run("tls", func(t *testing.T) {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"results":[]}`)
		}))
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.StartTLS()
		defer server.Close()

		provider := newSearxng(server.URL, time.Second, nil)
		_, err := provider.Search(context.Background(), redactionMarker, 5)
		if err == nil {
			t.Fatal("search against an untrusted certificate succeeded")
		}
		assertNoQuery(t, err.Error())
		if got := failureKind(err); got != "tls" {
			t.Fatalf("kind = %q, want tls: %v", got, err)
		}
	})
}

// TestTransportErrorMessageIsBounded locks the message shape: only the kind and
// host, even when the wrapped *url.Error names the full request URL.
func TestTransportErrorMessageIsBounded(t *testing.T) {
	raw := &url.Error{
		Op:  "Get",
		URL: "http://web-search:8080/search?q=" + url.QueryEscape(redactionMarker),
		Err: errors.New("connection refused"),
	}
	err := newTransportError("web-search:8080", "", raw)
	assertNoQuery(t, err.Error())
	if !strings.Contains(err.Error(), "web-search:8080") {
		t.Fatalf("error %q does not name the host", err)
	}
	if got := failureKind(err); got != "other" {
		t.Fatalf("kind = %q, want other", got)
	}
	if !errors.Is(err, raw) {
		t.Fatal("the original failure is no longer reachable with errors.Is")
	}
}

// TestSearchFailureLogOmitsTheQuery covers the Tool path for transport, status
// and body failures: the Warn line names the backend and kind, never the query.
func TestSearchFailureLogOmitsTheQuery(t *testing.T) {
	tests := []struct {
		name   string
		server func(t *testing.T) (baseURL string, timeout time.Duration)
		kind   string
	}{
		{
			name: "connect",
			server: func(t *testing.T) (string, time.Duration) {
				t.Helper()
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				baseURL := server.URL
				server.Close()
				return baseURL, 200 * time.Millisecond
			},
			kind: "connect",
		},
		{
			name: "timeout",
			server: func(t *testing.T) (string, time.Duration) {
				t.Helper()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					time.Sleep(100 * time.Millisecond)
					fmt.Fprint(w, `{"results":[]}`)
				}))
				t.Cleanup(server.Close)
				return server.URL, 20 * time.Millisecond
			},
			kind: "timeout",
		},
		{
			name: "status",
			server: func(t *testing.T) (string, time.Duration) {
				t.Helper()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, redactionMarker, http.StatusServiceUnavailable)
				}))
				t.Cleanup(server.Close)
				return server.URL, time.Second
			},
			kind: "http",
		},
		{
			name: "body read",
			server: func(t *testing.T) (string, time.Duration) {
				t.Helper()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, buf, err := w.(http.Hijacker).Hijack()
					if err != nil {
						return
					}
					defer conn.Close()
					// Promise more than is sent so the client's body read
					// fails after the marker has already left the server.
					fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n%s", redactionMarker)
					buf.Flush()
				}))
				t.Cleanup(server.Close)
				return server.URL, time.Second
			},
			kind: "http",
		},
		{
			name: "invalid JSON",
			server: func(t *testing.T) (string, time.Duration) {
				t.Helper()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, redactionMarker)
				}))
				t.Cleanup(server.Close)
				return server.URL, time.Second
			},
			kind: "http",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL, timeout := tt.server(t)
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			cfg := Config{
				Backend:    defaultBackend,
				BaseURL:    baseURL,
				Timeout:    config.Duration(timeout),
				Results:    defaultResults,
				MaxResults: defaultMaxResult,
			}
			provider, err := NewProvider(cfg)
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			search := New(cfg, provider, NewProvenance(), logger)
			_, err = search.Handler(context.Background(), In{Query: redactionMarker})
			if err == nil {
				t.Fatal("search succeeded, want a failure")
			}
			if got := tool.FailureMessage(err); got != searchUnavailable {
				t.Fatalf("failure message = %q, want %q", got, searchUnavailable)
			}
			assertNoQuery(t, err.Error())

			line := logs.String()
			if line == "" {
				t.Fatal("the failure was not logged")
			}
			assertNoQuery(t, line)
			if !strings.Contains(line, "backend=searxng") {
				t.Fatalf("log line does not name the backend: %q", line)
			}
			if !strings.Contains(line, "kind="+tt.kind) {
				t.Fatalf("log line does not name kind %q: %q", tt.kind, line)
			}
		})
	}
}
