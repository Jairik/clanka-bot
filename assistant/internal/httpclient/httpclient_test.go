package httpclient_test

import (
	"context"
	"fmt"
	"io"
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
