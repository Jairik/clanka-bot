package httpguard_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/httpguard"
)

func request(method, host string) *http.Request {
	req := httptest.NewRequest(method, "/", nil)
	req.Host = host
	return req
}

func TestHostsAcceptsLoopbackAndConfiguredNames(t *testing.T) {
	hosts, err := httpguard.NewHosts([]string{"Pi.Example.TS.net", "assistant.example:8443"})
	if err != nil {
		t.Fatalf("NewHosts: %v", err)
	}
	for _, host := range []string{
		"127.0.0.1:8082", "localhost", "localhost:8082", "[::1]:8082",
		"pi.example.ts.net", "PI.EXAMPLE.TS.NET:443", "assistant.example",
	} {
		if !hosts.Allows(request(http.MethodGet, host)) {
			t.Errorf("host %q was refused", host)
		}
	}
	for _, host := range []string{
		"", "attacker.example", "evil.pi.example.ts.net.evil.example", "pi.example.ts.net.evil.example",
	} {
		if hosts.Allows(request(http.MethodGet, host)) {
			t.Errorf("host %q was accepted", host)
		}
	}
}

func TestHostsZeroValueIsLoopbackOnly(t *testing.T) {
	var hosts httpguard.Hosts
	if !hosts.Allows(request(http.MethodGet, "127.0.0.1:8081")) {
		t.Fatal("loopback host was refused by the zero value")
	}
	if hosts.Allows(request(http.MethodGet, "attacker.example")) {
		t.Fatal("the zero value accepted a public host")
	}
}

func TestNewHostsRejectsPatternsAndURLs(t *testing.T) {
	for _, entry := range []string{
		"*.ts.net", "https://pi.example.ts.net", "pi.example.ts.net/path",
		"user@pi.example.ts.net", "pi example.ts.net", "*",
	} {
		if _, err := httpguard.NewHosts([]string{entry}); err == nil {
			t.Errorf("NewHosts accepted %q", entry)
		}
	}
}

func TestSameOriginRejectsCrossSiteFetchMetadata(t *testing.T) {
	req := request(http.MethodPost, "127.0.0.1:8082")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if httpguard.SameOrigin(req) {
		t.Fatal("cross-site request passed SameOrigin")
	}
	if httpguard.SameOriginOrLocal(req) {
		t.Fatal("cross-site request passed SameOriginOrLocal")
	}
}

func TestSameOriginAcceptsMatchingOriginAndForwardedHost(t *testing.T) {
	req := request(http.MethodPost, "127.0.0.1:8082")
	req.Header.Set("Origin", "https://pi.example.ts.net")
	req.Header.Set("X-Forwarded-Host", "pi.example.ts.net")
	if !httpguard.SameOrigin(req) {
		t.Fatal("matching forwarded origin was refused")
	}
}

func TestSameOriginRequiresAnOriginForBrowsers(t *testing.T) {
	req := request(http.MethodPost, "127.0.0.1:8082")
	if httpguard.SameOrigin(req) {
		t.Fatal("a mutation with no origin passed the strict browser guard")
	}
	if !httpguard.SameOriginOrLocal(req) {
		t.Fatal("a local client with no origin was refused")
	}
}

func TestSafeMethodsPassBothGuards(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := request(method, "attacker.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		if !httpguard.SameOrigin(req) || !httpguard.SameOriginOrLocal(req) {
			t.Errorf("%s was treated as a mutation", method)
		}
	}
}

func TestIsJSONAcceptsApplicationJSONWithParameters(t *testing.T) {
	req := request(http.MethodPost, "127.0.0.1:8081")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if !httpguard.IsJSON(req) {
		t.Fatal("application/json with charset was refused")
	}
	req.Header.Set("Content-Type", "text/plain")
	if httpguard.IsJSON(req) {
		t.Fatal("text/plain was accepted as JSON")
	}
	if !httpguard.NeedsJSONBody(http.MethodPost) || !httpguard.NeedsJSONBody(http.MethodPut) {
		t.Fatal("POST/PUT should need a JSON body")
	}
	if httpguard.NeedsJSONBody(http.MethodDelete) {
		t.Fatal("DELETE should not need a JSON body")
	}
}
