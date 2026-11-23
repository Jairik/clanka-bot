package settings_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// liveAgentKeys are the Dashboard-writable Agent-path settings a later Hermes
// cutover must keep loading under the same key, or through an explicit alias.
// YAML-only agent.providers / timeouts are not in this list because they are
// not SQLite rows.
func liveAgentKeys() []settings.Definition {
	return append([]settings.Definition{
		agent.ModelDefinition("code/model", "config/model"),
		settings.Strings(wording.PrefixesKey, []string{"ask the agent"},
			settings.WithConfig([]string{"ask the agent", "agent", "hey agent"}),
			settings.WithValidator(func(value any) error {
				list, ok := value.([]string)
				if !ok {
					return fmt.Errorf("want a list of prefixes, got %T", value)
				}
				for _, text := range list {
					if strings.TrimSpace(text) == "" {
						return fmt.Errorf("a prefix must not be blank")
					}
				}
				return nil
			}),
		),
		settings.String(wording.FillerKey, wording.DefaultFiller,
			settings.WithConfig("One moment."),
			settings.WithValidator(func(value any) error {
				if strings.TrimSpace(value.(string)) == "" {
					return fmt.Errorf("must not be blank")
				}
				return nil
			}),
		),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage,
			settings.WithConfig("Sorry, I can't answer that right now."),
			settings.WithValidator(func(value any) error {
				if strings.TrimSpace(value.(string)) == "" {
					return fmt.Errorf("must not be blank")
				}
				return nil
			}),
		),
		settings.String(wording.ClarifyKey, agent.DefaultClarifyMessage,
			settings.WithConfig(agent.DefaultClarifyMessage),
			settings.WithValidator(func(value any) error {
				if strings.TrimSpace(value.(string)) == "" {
					return fmt.Errorf("must not be blank")
				}
				return nil
			}),
		),
	}, agent.LimitsDefinitions()...)
}

// TestCutoverKeepsStoredAgentOverrides is the settings-migration contract for
// #526: a Pi that already stored Agent wording, model and spoken-budget
// overrides must still boot with those values after a later cutover, as long
// as the Definitions remain.
func TestCutoverKeepsStoredAgentOverrides(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := openSettings(t, liveAgentKeys()...)

	want := map[string]any{
		agent.ModelKey:               "openrouter/cutover-model",
		wording.PrefixesKey:          []string{"ask the agent", "please reason"},
		wording.FillerKey:            "Just a second.",
		wording.UnavailableKey:       "The fallback is offline.",
		wording.ClarifyKey:           "Say that again.",
		agent.SpokenMaxSentencesKey:  3,
		agent.SpokenMaxCharsKey:      180,
		agent.MaxCompletionTokensKey: 250,
	}
	if err := svc.SetMany(ctx, want); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	for key, stored := range want {
		value, ok := svc.Get(key)
		if !ok {
			t.Fatalf("%s is not defined", key)
		}
		if value.Source != settings.SourceOverride {
			t.Errorf("%s source = %q, want override", key, value.Source)
		}
		got, err := json.Marshal(value.Value)
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		wantRaw, err := json.Marshal(stored)
		if err != nil {
			t.Fatalf("marshal want %s: %v", key, err)
		}
		if string(got) != string(wantRaw) {
			t.Errorf("%s = %s, want %s", key, got, wantRaw)
		}
	}
}

// TestCutoverDroppingUnusedAgentModelLeavesOtherOverrides is the orphan-safe
// removal rule: dropping agent.model (the likely unused provider UI after
// Hermes owns the model) must not fail startup and must not drop wording or
// spoken-budget overrides.
func TestCutoverDroppingUnusedAgentModelLeavesOtherOverrides(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := openSettings(t, liveAgentKeys()...)
	if err := svc.SetMany(ctx, map[string]any{
		agent.ModelKey:      "openrouter/old-model",
		wording.FillerKey:   "Hold on.",
		wording.PrefixesKey: []string{"ask the agent"},
	}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	surviving := make([]settings.Definition, 0, len(liveAgentKeys())-1)
	for _, def := range liveAgentKeys() {
		if def.Key == agent.ModelKey {
			continue
		}
		surviving = append(surviving, def)
	}
	reopened, err := settings.New(ctx, st, surviving...)
	if err != nil {
		t.Fatalf("New after dropping %s: %v", agent.ModelKey, err)
	}
	if _, ok := reopened.Get(agent.ModelKey); ok {
		t.Fatal("dropped agent.model is still defined")
	}
	if got := reopened.String(wording.FillerKey); got != "Hold on." {
		t.Errorf("filler = %q, want the stored override", got)
	}
	prefixes, _ := reopened.Get(wording.PrefixesKey)
	list, _ := prefixes.Value.([]string)
	if len(list) != 1 || list[0] != "ask the agent" {
		t.Errorf("prefixes = %#v, want the stored override", prefixes.Value)
	}

	var leftover string
	if err := st.DB().QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, agent.ModelKey).Scan(&leftover); err != nil {
		t.Fatalf("orphan row missing: %v", err)
	}
	if leftover != `"openrouter/old-model"` {
		t.Errorf("orphan agent.model = %s, want the original JSON", leftover)
	}
}

// TestCutoverTightenedPrefixValidatorIgnoresTheStoredRow pins the ignored-
// override path a wording change must use instead of failing boot: a stored
// prefix list that fails a later validator falls back to config, stays in
// SQLite, and is listed on Ignored() so the owner can reset it.
func TestCutoverTightenedPrefixValidatorIgnoresTheStoredRow(t *testing.T) {
	ctx := context.Background()
	svc, st, err := openSeeded(t,
		map[string]string{wording.PrefixesKey: `["ask the agent",""]`},
		settings.Strings(wording.PrefixesKey, []string{"ask the agent"},
			settings.WithConfig([]string{"ask the agent"}),
			settings.WithValidator(func(value any) error {
				list, _ := value.([]string)
				for _, text := range list {
					if strings.TrimSpace(text) == "" {
						return fmt.Errorf("a prefix must not be blank")
					}
				}
				return nil
			}),
		),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig("One moment.")),
	)
	if err != nil {
		t.Fatalf("New failed on a stale prefix list: %v", err)
	}
	prefixes, ok := svc.Get(wording.PrefixesKey)
	if !ok {
		t.Fatal("agent.request_prefixes is not defined")
	}
	list, _ := prefixes.Value.([]string)
	if prefixes.Source != settings.SourceConfig || len(list) != 1 || list[0] != "ask the agent" {
		t.Fatalf("prefixes = %+v, want the config layer", prefixes)
	}
	ignored := svc.Ignored()
	if len(ignored) != 1 || ignored[0].Key != wording.PrefixesKey {
		t.Fatalf("Ignored() = %+v, want the skipped prefixes row", ignored)
	}
	if got := svc.String(wording.FillerKey); got != "One moment." {
		t.Errorf("unrelated filler = %q, want the config layer", got)
	}

	// The ignored row is still there for a later compatible build.
	var raw string
	if err := st.DB().QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, wording.PrefixesKey).Scan(&raw); err != nil {
		t.Fatalf("kept row: %v", err)
	}
	if raw != `["ask the agent",""]` {
		t.Errorf("stored prefixes = %s, want the original JSON", raw)
	}
}

// TestCutoverLeavesHermesGrantsAlone is the boundary with #525: dropping or
// rewriting Agent-path Definitions must not clear stored Hermes account grants.
func TestCutoverLeavesHermesGrantsAlone(t *testing.T) {
	ctx := context.Background()
	grant := integrations.GrantKey("github")
	defs := append(liveAgentKeys(), integrations.Definitions()...)
	svc, st, _ := openSettings(t, defs...)
	if _, err := svc.Set(ctx, grant, true); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	if _, err := svc.Set(ctx, wording.FillerKey, "Hold on."); err != nil {
		t.Fatalf("seed filler: %v", err)
	}

	reopened, err := settings.New(ctx, st, integrations.Definitions()...)
	if err != nil {
		t.Fatalf("New with only Hermes grants: %v", err)
	}
	if !reopened.Bool(grant) {
		t.Fatal("hermes.integrations.github was cleared while Agent definitions were dropped")
	}
	if _, ok := reopened.Get(wording.FillerKey); ok {
		t.Fatal("dropped agent.filler is still defined")
	}
}
