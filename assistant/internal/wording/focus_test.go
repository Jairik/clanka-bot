package wording_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// TestIntentPhrasesAreLiveAndResettable keeps the shared Focus phrases on the
// same live settings path as a Tool's phrases: a stored override applies on the
// next request and Reset restores the config layer.
func TestIntentPhrasesAreLiveAndResettable(t *testing.T) {
	f := newFixture(t, nil, nil)
	ctx := context.Background()
	f.manager.Start(ctx, nil)
	key := wording.IntentPhrasesKey(focus.Stop)

	if got := f.manager.Routing().IntentPhrases["stop"]; got != "" {
		t.Fatalf("initial stop phrase = %q, want none in the test config", got)
	}
	if _, err := f.settings.Set(ctx, key, []string{"stop", "halt"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	words := f.manager.Routing()
	if words.IntentPhrases["stop"] != "stop" || words.IntentPhrases["halt"] != "stop" {
		t.Fatalf("intent phrases = %#v, want stop and halt", words.IntentPhrases)
	}
	if owner, ok := words.ControlPhrases["stop"]; ok {
		t.Fatalf("stop was also routed to Tool %q", owner)
	}

	reset, err := f.settings.Reset(ctx, key)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got, _ := reset.Value.([]string); len(got) != 0 {
		t.Fatalf("after Reset = %v, want the empty default", got)
	}
	if _, ok := f.manager.Routing().IntentPhrases["stop"]; ok {
		t.Fatal("a reset phrase still routes")
	}
}

// TestBuildAcceptsAnIntentPhrase checks the config layer is split by target
// kind, so an intent phrase becomes an intent setting rather than a Tool error.
func TestBuildAcceptsAnIntentPhrase(t *testing.T) {
	cfg := testConfig()
	cfg.ControlPhrases = map[string]string{"stop": "stop", "louder": "test.clock"}
	setup, err := wording.Build(testRegistry(t), cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The config table is the config layer, not the code default; read the
	// effective value through one settings service.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	svc, err := settings.New(context.Background(), st, setup.Definitions...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	value, ok := svc.Get(wording.IntentPhrasesKey(focus.Stop))
	if !ok {
		t.Fatal("no live setting for the stop intent")
	}
	if got, _ := value.Value.([]string); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("stop config = %#v, want the config phrase", value.Value)
	}
	clock, _ := svc.Get(wording.ControlPhrasesKey("test.clock"))
	if got, _ := clock.Value.([]string); len(got) != 1 || got[0] != "louder" {
		t.Fatalf("clock config = %#v, want the Tool phrase", clock.Value)
	}
}

// TestBuildRejectsAnUnknownIntent keeps a typo out of the live table.
func TestBuildRejectsAnUnknownIntent(t *testing.T) {
	cfg := testConfig()
	cfg.ControlPhrases = map[string]string{"stop": "stopp"}
	_, err := wording.Build(testRegistry(t), cfg)
	if err == nil {
		t.Fatal("Build accepted an unknown intent")
	}
	if !strings.Contains(err.Error(), "unknown intent") {
		t.Fatalf("error = %q, want it to name the intent", err)
	}
}

// TestRoutingDropsAPhraseClaimedByAToolAndAnIntent keeps ambiguity honest: one
// phrase cannot both call a Tool and act on Focus, so it falls through.
func TestRoutingDropsAPhraseClaimedByAToolAndAnIntent(t *testing.T) {
	f := newFixture(t, nil, nil)
	ctx := context.Background()
	f.manager.Start(ctx, nil)

	if err := f.settings.SetMany(ctx, map[string]any{
		wording.ControlPhrasesKey("test.clock"): []string{"shared words"},
		wording.IntentPhrasesKey(focus.Stop):    []string{"shared words"},
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	words := f.manager.Routing()
	if owner, ok := words.ControlPhrases["shared words"]; ok {
		t.Fatalf("shared phrase routed to Tool %q", owner)
	}
	if intent, ok := words.IntentPhrases["shared words"]; ok {
		t.Fatalf("shared phrase routed to intent %q", intent)
	}
}
