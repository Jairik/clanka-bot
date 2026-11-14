package spotify_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	spotifytools "github.com/Jairik/clanka-bot/assistant/tools/spotify"
)

// fakeStore is a write-only secrets stand-in. The module only needs the
// interfaces, not a database.
type fakeStore struct{}

func (fakeStore) Get(string) (string, bool) { return "", false }
func (fakeStore) Metadata(name string) (secrets.Metadata, bool) {
	return secrets.Metadata{Name: name, Source: secrets.SourceNone}, true
}
func (fakeStore) Set(context.Context, string, string) (secrets.Metadata, error) {
	return secrets.Metadata{}, nil
}
func (fakeStore) Clear(context.Context, string) (secrets.Metadata, error) {
	return secrets.Metadata{}, nil
}

func TestModuleDeclaresTheWebAPIRefreshToken(t *testing.T) {
	module := spotifytools.NewModule()
	if module.Name() != "spotify" {
		t.Fatalf("name = %q", module.Name())
	}
	names := module.RequiredSecrets()
	if len(names) != 1 || names[0] != spotify.SecretName {
		t.Fatalf("RequiredSecrets = %v", names)
	}
}

func TestModuleBuildsTheSharedAuthSeam(t *testing.T) {
	cfg := spotify.DefaultConfig()
	cfg.ClientID = "0123456789abcdef0123456789abcdef"
	cfg.RedirectURI = "http://127.0.0.1:8082/spotify/callback"
	module := spotifytools.NewModule()
	module.BindConfig(cfg)
	if err := module.Register(tool.NewRegistry(time.Second), tool.Deps{Secrets: fakeStore{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth := module.Auth()
	if auth == nil {
		t.Fatal("Auth() is nil")
	}
	if got := auth.Config(); got.ClientID != cfg.ClientID || got.RedirectURI != cfg.RedirectURI {
		t.Fatalf("auth config = %+v", got)
	}
	if module.Client() == nil {
		t.Fatal("Client() is nil")
	}
}

// TestModuleStartIsNonBlockingAndCloseJoinsTheRefresh drives the module hooks
// with a fake Web API and no refresh token, so nothing leaves the process and
// the refresh fails fast; the point is that Start returns and Close joins.
func TestModuleStartIsNonBlockingAndCloseJoinsTheRefresh(t *testing.T) {
	cfg := spotify.DefaultConfig()
	cfg.ClientID = "0123456789abcdef0123456789abcdef"
	cfg.RedirectURI = "http://127.0.0.1:8082/spotify/callback"
	cfg.APIBaseURL = "http://127.0.0.1:1"
	cfg.TokenURL = "http://127.0.0.1:1/token"
	module := spotifytools.NewModule()
	module.BindConfig(cfg)
	if err := module.Register(tool.NewRegistry(time.Second), tool.Deps{Secrets: fakeStore{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	done := make(chan struct{})
	go func() {
		if err := module.Start(t.Context()); err != nil {
			t.Errorf("Start: %v", err)
		}
		module.Close()
		module.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start or Close blocked")
	}
}

func TestModuleStartDoesNothingWhileTheWebAPIIsOff(t *testing.T) {
	module := spotifytools.NewModule() // default config: no client ID
	if err := module.Register(tool.NewRegistry(time.Second), tool.Deps{Secrets: fakeStore{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := module.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	module.Close()
}
