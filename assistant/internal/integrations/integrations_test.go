package integrations_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

func newFixture(t *testing.T) (*integrations.Service, *secrets.Service, *settings.Service) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	names := []string{"GITHUB_TOKEN", "GITHUB_NOTIFICATIONS_TOKEN", "OPENROUTER_API_KEY", "SPOTIFY_REFRESH_TOKEN", "NTFY_TOPIC", "NTFY_TOKEN", "WEATHERAPI_API_KEY", "MONSTER_EMAIL", "MONSTER_PASSWORD"}
	secretSvc, err := secrets.New(ctx, st.DB(), names)
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	setSvc, err := settings.New(ctx, st, integrations.Definitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return integrations.New(secretSvc, setSvc), secretSvc, setSvc
}

func TestCatalogAndDefinitionsAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, account := range integrations.Catalog {
		if account.ID == "" || account.Name == "" || account.Summary == "" {
			t.Errorf("incomplete account %+v", account)
		}
		if seen[account.ID] {
			t.Errorf("duplicate account id %q", account.ID)
		}
		seen[account.ID] = true
		if len(account.Secrets) == 0 {
			t.Errorf("%s has no required secret", account.ID)
		}
	}
	defs := integrations.Definitions()
	if len(defs) != len(integrations.Catalog) {
		t.Fatalf("definitions = %d, want %d", len(defs), len(integrations.Catalog))
	}
	keys := make(map[string]bool, len(defs))
	for _, def := range defs {
		keys[def.Key] = true
	}
	for _, account := range integrations.Catalog {
		if !keys[integrations.GrantKey(account.ID)] {
			t.Errorf("%s has no grant definition", account.ID)
		}
	}
}

func TestLinkUnlinkAndStatus(t *testing.T) {
	svc, _, _ := newFixture(t)
	ctx := context.Background()

	view, ok := svc.Get("github")
	if !ok {
		t.Fatal("github not in catalog")
	}
	if view.Status != integrations.StatusMissing {
		t.Fatalf("initial status = %q, want missing", view.Status)
	}
	if err := svc.Link(ctx, "github", map[string]string{"GITHUB_TOKEN": "ghp_example_value"}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	view, _ = svc.Get("github")
	if view.Status != integrations.StatusLinked {
		t.Fatalf("linked status = %q, want linked", view.Status)
	}
	if !view.Secrets[0].Set {
		t.Fatal("GITHUB_TOKEN not marked set")
	}
	if err := svc.Unlink(ctx, "github"); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	view, _ = svc.Get("github")
	if view.Status != integrations.StatusMissing {
		t.Fatalf("unlinked status = %q, want missing", view.Status)
	}
}

func TestEnvironmentValuesAreReportedButNotStored(t *testing.T) {
	t.Setenv("WEATHERAPI_API_KEY", "env-weather-key")
	svc, secretSvc, _ := newFixture(t)
	view, ok := svc.Get("weather")
	if !ok {
		t.Fatal("weather not in catalog")
	}
	if view.Status != integrations.StatusEnvironment {
		t.Fatalf("status = %q, want environment", view.Status)
	}
	meta, _ := secretSvc.Metadata("WEATHERAPI_API_KEY")
	if meta.Source != secrets.SourceEnv {
		t.Fatalf("source = %q, want env", meta.Source)
	}
}

func TestLinkRejectsMissingAndUnknownFields(t *testing.T) {
	svc, _, _ := newFixture(t)
	ctx := context.Background()
	if err := svc.Link(ctx, "github", nil); err == nil {
		t.Fatal("missing required secret succeeded")
	}
	if err := svc.Link(ctx, "github", map[string]string{"GITHUB_TOKEN": "x", "NOT_A_SECRET": "y"}); err == nil {
		t.Fatal("unknown field succeeded")
	}
	if err := svc.Link(ctx, "nope", map[string]string{}); err == nil {
		t.Fatal("unknown account succeeded")
	}
	// Nothing was written by a refused link.
	view, _ := svc.Get("github")
	if view.Status != integrations.StatusMissing {
		t.Fatalf("status after refusal = %q, want missing", view.Status)
	}
}

func TestGrantIsPerAccountAndRevocable(t *testing.T) {
	svc, _, _ := newFixture(t)
	ctx := context.Background()
	if got := svc.Granted(); len(got) != 0 {
		t.Fatalf("initial grants = %v", got)
	}
	if err := svc.SetGrant(ctx, "github", true); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	if err := svc.SetGrant(ctx, "spotify", true); err != nil {
		t.Fatalf("SetGrant spotify: %v", err)
	}
	if got := svc.Granted(); len(got) != 2 || got[0] != "github" || got[1] != "spotify" {
		t.Fatalf("grants = %v, want catalog order", got)
	}
	if err := svc.SetGrant(ctx, "github", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := svc.Granted(); len(got) != 1 || got[0] != "spotify" {
		t.Fatalf("grants after revoke = %v", got)
	}
	if err := svc.SetGrant(ctx, "nope", true); err == nil {
		t.Fatal("unknown account grant succeeded")
	}
}

func TestLinkAllowsBlankRequiredWhenAlreadyStored(t *testing.T) {
	svc, secretSvc, _ := newFixture(t)
	ctx := context.Background()
	if err := svc.Link(ctx, "github", map[string]string{"GITHUB_TOKEN": "ghp_first_value_long_enough"}); err != nil {
		t.Fatalf("first Link: %v", err)
	}
	// Adding the optional token must not force the stored required one to be
	// retyped.
	if err := svc.Link(ctx, "github", map[string]string{
		"GITHUB_TOKEN":               "",
		"GITHUB_NOTIFICATIONS_TOKEN": "ghp_notify",
	}); err != nil {
		t.Fatalf("second Link: %v", err)
	}
	if meta, _ := secretSvc.Metadata("GITHUB_TOKEN"); !meta.Set {
		t.Fatalf("GITHUB_TOKEN was cleared: %+v", meta)
	}
	if meta, _ := secretSvc.Metadata("GITHUB_NOTIFICATIONS_TOKEN"); !meta.Set {
		t.Fatal("GITHUB_NOTIFICATIONS_TOKEN was not stored")
	}
}

func TestUnlinkLeavesEnvironmentValueActive(t *testing.T) {
	t.Setenv("WEATHERAPI_API_KEY", "env-weather-key")
	svc, _, _ := newFixture(t)
	if err := svc.Unlink(context.Background(), "weather"); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	view, _ := svc.Get("weather")
	if view.Status != integrations.StatusEnvironment {
		t.Fatalf("status after unlink = %q, want environment", view.Status)
	}
	if !view.Secrets[0].Set {
		t.Fatal("environment value was reported as cleared")
	}
}
