package httpclient_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
)

// TestRequireSecureOriginSeparatesSecretsFromClearText pins the shared rule:
// HTTPS always, plain HTTP only for a loopback fake or a deliberate opt-in.
func TestRequireSecureOriginSeparatesSecretsFromClearText(t *testing.T) {
	allowed := []struct {
		name          string
		url           string
		allowInsecure bool
	}{
		{"public https", "https://api.github.com", false},
		{"enterprise https base path", "https://ghe.example/api/v3", false},
		{"loopback ipv4", "http://127.0.0.1:1234/api/v3", false},
		{"loopback anywhere in 127/8", "http://127.42.0.9", false},
		{"loopback named localhost", "http://localhost:8080/v1/current.json", false},
		{"loopback named LOCALHOST", "http://LOCALHOST:8080/x", false},
		{"loopback ipv6", "http://[::1]:1234/x", false},
		{"mapped loopback ipv6", "http://[::ffff:127.0.0.1]/x", false},
		{"opt-in LAN origin", "http://ghe.example/api/v3", true},
		{"opt-in private ip", "http://192.168.1.20/api/v3", true},
		{"opt-in with https", "https://ghe.example/api/v3", true},
	}
	for _, tt := range allowed {
		t.Run("allowed "+tt.name, func(t *testing.T) {
			if err := httpclient.RequireSecureOrigin(tt.url, tt.allowInsecure); err != nil {
				t.Fatalf("RequireSecureOrigin(%q, %v) = %v, want nil", tt.url, tt.allowInsecure, err)
			}
		})
	}

	refused := []string{
		"http://ghe.example/api/v3",
		"http://api.github.com",
		"http://192.168.1.20/api/v3",
		"http://10.0.0.5:8080/x",
		"http://127.0.0.1.evil.example/x",
		"http://localhost.evil.example/x",
	}
	for _, raw := range refused {
		t.Run("refused "+raw, func(t *testing.T) {
			err := httpclient.RequireSecureOrigin(raw, false)
			if !errors.Is(err, httpclient.ErrInsecureOrigin) {
				t.Fatalf("RequireSecureOrigin(%q) = %v, want ErrInsecureOrigin", raw, err)
			}
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("error echoed the configured URL: %v", err)
			}
		})
	}
}

// TestRequireSecureOriginRefusesMalformedURLs keeps a broken operator value at
// Configure instead of a request-time surprise. The message never repeats the
// raw value, which may contain userinfo.
func TestRequireSecureOriginRefusesMalformedURLs(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"api.github.com",
		"ftp://ghe.example/api/v3",
		"https://",
		"http://",
		"//ghe.example/api/v3",
		"http://[::1",
		"http://user:hunter2@[::1",
	} {
		t.Run(raw, func(t *testing.T) {
			err := httpclient.RequireSecureOrigin(raw, true)
			if err == nil {
				t.Fatalf("RequireSecureOrigin(%q) accepted a malformed URL", raw)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "ghe.example") {
				t.Fatalf("error echoed the configured URL: %v", err)
			}
		})
	}
}

// TestInsecureOriginNamesOnlyClearTextLANOrigins keeps the startup warning
// honest: a secure origin and a loopback fake are not clear-text risks.
func TestInsecureOriginNamesOnlyClearTextLANOrigins(t *testing.T) {
	for _, raw := range []string{
		"http://ghe.example/api/v3",
		"http://192.168.1.20/api/v3",
	} {
		if !httpclient.InsecureOrigin(raw) {
			t.Fatalf("InsecureOrigin(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"https://ghe.example/api/v3",
		"https://api.github.com",
		"http://127.0.0.1:1234/api/v3",
		"http://localhost:8080/x",
		"http://[::1]:1234/x",
		"not a url",
		"",
	} {
		if httpclient.InsecureOrigin(raw) {
			t.Fatalf("InsecureOrigin(%q) = true, want false", raw)
		}
	}
}
