package httpclient_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
)

func TestNewAppliesKeepAliveDefaults(t *testing.T) {
	client := httpclient.New(httpclient.Options{Timeout: 250 * time.Millisecond})
	if client.Timeout != 250*time.Millisecond {
		t.Fatalf("Timeout = %s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.MaxIdleConns == 0 || transport.MaxIdleConnsPerHost == 0 {
		t.Fatalf("idle pool = %d/%d, want a keep-alive pool",
			transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout == 0 || transport.ResponseHeaderTimeout == 0 {
		t.Fatal("connection and header timeouts must be bounded")
	}
}

func TestNewHonoursNoProxyAndCustomDial(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	called := false
	client := httpclient.New(httpclient.Options{
		Timeout: time.Second,
		NoProxy: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			called = true
			return nil, fmt.Errorf("custom dial")
		},
	})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	// A proxy would connect on the caller's behalf, so the SSRF dial policy
	// must see the real destination even when the environment names one.
	if transport.Proxy != nil {
		t.Fatal("NoProxy left an environment proxy in the transport")
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test/", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := client.Do(req); err == nil {
		t.Fatal("request succeeded, want the custom dial error")
	}
	if !called {
		t.Fatal("the custom DialContext was not used")
	}
}

func TestNewReusesOneConnection(t *testing.T) {
	var mu sync.Mutex
	var peers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		peers = append(peers, r.RemoteAddr)
		mu.Unlock()
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	client := httpclient.New(httpclient.Options{Timeout: time.Second})
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(peers); i++ {
		if peers[i] != peers[0] {
			t.Fatalf("peers = %v, want one kept-alive connection", peers)
		}
	}
}
