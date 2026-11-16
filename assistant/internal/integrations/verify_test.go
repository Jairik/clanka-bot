package integrations_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/monster"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

// fakeSecrets is an in-memory SecretReader for tests.
type fakeSecrets map[string]string

func (f fakeSecrets) Get(name string) (string, bool) {
	v, ok := f[name]
	return v, ok
}

// statusServer returns the given status for every request. When closed is
// true the listener is shut so requests fail with connection refused.
func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
}

// directVerifier builds a Verifier whose direct-HTTP providers all point at the
// same test server, with a stored key for each.
func directVerifier(secrets integrations.SecretReader, serverURL string) *integrations.Verifier {
	return integrations.NewVerifier(integrations.VerifierOptions{
		Secrets:        secrets,
		GitHubBase:     serverURL,
		OpenRouterBase: serverURL,
		WeatherURL:     serverURL + "/current.json",
		NtfyBase:       serverURL,
		Timeout:        0, // no timeout in tests
	})
}

func TestVerifyDirectHTTPProviders(t *testing.T) {
	secrets := fakeSecrets{
		"GITHUB_TOKEN":           "ghp_x",
		"OPENROUTER_API_KEY":      "sk-or-x",
		"WEATHERAPI_API_KEY":      "wx",
		"NTFY_TOPIC":              "topic-x",
		"NTFY_TOKEN":              "ntfy-token",
	}
	// 200 -> ok for every direct-HTTP account.
	t.Run("ok", func(t *testing.T) {
		srv := statusServer(t, http.StatusOK)
		defer srv.Close()
		v := directVerifier(secrets, srv.URL)
		for _, id := range []string{"github", "openrouter", "weather", "notify"} {
			res, err := v.Verify(context.Background(), id)
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if res.Class != integrations.VerifyOK {
				t.Fatalf("%s: got class %q, want ok", id, res.Class)
			}
		}
	})

	// 401 -> rejected for every direct-HTTP account.
	t.Run("rejected", func(t *testing.T) {
		srv := statusServer(t, http.StatusUnauthorized)
		defer srv.Close()
		v := directVerifier(secrets, srv.URL)
		for _, id := range []string{"github", "openrouter", "weather", "notify"} {
			res, err := v.Verify(context.Background(), id)
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if res.Class != integrations.VerifyRejected {
				t.Fatalf("%s: got class %q, want rejected", id, res.Class)
			}
		}
	})

	// Connection refused -> unreachable for every direct-HTTP account.
	t.Run("unreachable", func(t *testing.T) {
		srv := statusServer(t, http.StatusOK)
		url := srv.URL
		srv.Close() // now every request fails to connect
		v := directVerifier(secrets, url)
		for _, id := range []string{"github", "openrouter", "weather", "notify"} {
			res, err := v.Verify(context.Background(), id)
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if res.Class != integrations.VerifyUnreachable {
				t.Fatalf("%s: got class %q, want unreachable", id, res.Class)
			}
		}
	})
}

func TestVerifyWeather200WithErrorBodyIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"code":1002,"message":"API key not provided."}}`))
	}))
	defer srv.Close()
	v := directVerifier(fakeSecrets{"WEATHERAPI_API_KEY": "wx"}, srv.URL)
	res, err := v.Verify(context.Background(), "weather")
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != integrations.VerifyRejected {
		t.Fatalf("weather 200-with-error: got class %q, want rejected", res.Class)
	}
}

func TestVerifyNotConfigured(t *testing.T) {
	srv := statusServer(t, http.StatusOK)
	defer srv.Close()
	// No secrets at all.
	v := directVerifier(fakeSecrets{}, srv.URL)
	for _, id := range []string{"github", "openrouter", "weather", "notify"} {
		res, err := v.Verify(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if res.Class != integrations.VerifyNotConfigured {
			t.Fatalf("%s: got class %q, want not-configured", id, res.Class)
		}
	}
}

// fakeSpotify returns the configured error from Verify so the verifier's
// mapping branch is exercised without a real token manager.
type fakeSpotify struct{ err error }

func (f fakeSpotify) Verify(context.Context) error { return f.err }

func TestVerifySpotify(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		v := integrations.NewVerifier(integrations.VerifierOptions{Spotify: fakeSpotify{err: nil}})
		res, err := v.Verify(context.Background(), "spotify")
		if err != nil || res.Class != integrations.VerifyOK {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		v := integrations.NewVerifier(integrations.VerifierOptions{
			Spotify: fakeSpotify{err: &spotify.APIError{StatusCode: http.StatusUnauthorized}},
		})
		res, err := v.Verify(context.Background(), "spotify")
		if err != nil || res.Class != integrations.VerifyRejected {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		v := integrations.NewVerifier(integrations.VerifierOptions{Spotify: fakeSpotify{err: errors.New("boom")}})
		res, err := v.Verify(context.Background(), "spotify")
		if err != nil || res.Class != integrations.VerifyUnreachable {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		v := integrations.NewVerifier(integrations.VerifierOptions{Spotify: fakeSpotify{err: spotify.ErrNotConfigured}})
		res, err := v.Verify(context.Background(), "spotify")
		if err != nil || res.Class != integrations.VerifyNotConfigured {
			t.Fatalf("got %v %v", res, err)
		}
	})
}

// fakeMonsterAPI is the minimal monster.API seam for the verify tests.
type fakeMonsterAPI struct {
	devices    []monster.Device
	devicesErr error
}

func (f *fakeMonsterAPI) Devices(context.Context, string, string) ([]monster.Device, error) {
	return f.devices, f.devicesErr
}
func (f *fakeMonsterAPI) Properties(context.Context, string, string, string) (map[string]any, error) {
	return nil, nil
}
func (f *fakeMonsterAPI) Set(context.Context, string, string, string, string, any) error { return nil }

func TestVerifyMonster(t *testing.T) {
	creds := func() (string, string, bool) { return "owner@example.com", "secret", true }
	t.Run("ok", func(t *testing.T) {
		svc := monster.NewService(monster.Options{API: &fakeMonsterAPI{devices: []monster.Device{{DSN: "DSN1"}}}, Credentials: creds})
		v := integrations.NewVerifier(integrations.VerifierOptions{Monster: svc})
		res, err := v.Verify(context.Background(), "monster")
		if err != nil || res.Class != integrations.VerifyOK {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		svc := monster.NewService(monster.Options{API: &fakeMonsterAPI{devicesErr: monster.ErrCredentials}, Credentials: creds})
		v := integrations.NewVerifier(integrations.VerifierOptions{Monster: svc})
		res, err := v.Verify(context.Background(), "monster")
		if err != nil || res.Class != integrations.VerifyRejected {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		svc := monster.NewService(monster.Options{API: &fakeMonsterAPI{devicesErr: errors.New("connection refused")}, Credentials: creds})
		v := integrations.NewVerifier(integrations.VerifierOptions{Monster: svc})
		res, err := v.Verify(context.Background(), "monster")
		if err != nil || res.Class != integrations.VerifyUnreachable {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		svc := monster.NewService(monster.Options{API: &fakeMonsterAPI{}, Credentials: func() (string, string, bool) { return "", "", false }})
		v := integrations.NewVerifier(integrations.VerifierOptions{Monster: svc})
		res, err := v.Verify(context.Background(), "monster")
		if err != nil || res.Class != integrations.VerifyNotConfigured {
			t.Fatalf("got %v %v", res, err)
		}
	})
}

func TestVerifyUnknownAccount(t *testing.T) {
	v := integrations.NewVerifier(integrations.VerifierOptions{Secrets: fakeSecrets{}})
	if _, err := v.Verify(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown account")
	}
}
