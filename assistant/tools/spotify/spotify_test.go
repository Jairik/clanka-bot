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
