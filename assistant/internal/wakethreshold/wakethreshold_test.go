package wakethreshold_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/wakethreshold"
)

// writeConfig writes a temporary config.yaml for testing.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// openSettings opens a temporary settings store and service with given definitions.
func openSettings(t *testing.T, defs ...settings.Definition) *settings.Service {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	service, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return service
}

// TestThresholdPrecedenceAndReset is the acceptance path: a stored value wins
// over WAKEWORD_THRESHOLDS, Reset returns to the environment value, and a
// model that inherits the code default reports that layer instead.
func TestThresholdPrecedenceAndReset(t *testing.T) {
	t.Setenv("ASSISTANT_WAKEWORD_THRESHOLDS", "clanker:0.62")
	cfg, err := config.Load(writeConfig(t, `
wakeword:
  addr: "wakeword:10400"
  models: ["clanker", "okay_nabu"]
stt: {addr: "stt:10300"}
`))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	service := openSettings(t, wakethreshold.Definitions(cfg.Wakeword)...)
	ctx := context.Background()

	value, ok := service.Get(wakethreshold.Key("clanker"))
	if !ok {
		t.Fatal("clanker threshold is not defined")
	}
	if value.Value != 0.62 || value.Source != settings.SourceConfig {
		t.Fatalf("clanker = %v from %s, want 0.62 from the environment", value.Value, value.Source)
	}
	// A model the environment does not name stays on the code default, so a
	// Reset cannot silently invent a config-layer value.
	inherited, _ := service.Get(wakethreshold.Key("okay_nabu"))
	if inherited.Value != config.DefaultWakeThreshold || inherited.Source != settings.SourceDefault {
		t.Fatalf("okay_nabu = %v from %s, want the code default %v",
			inherited.Value, inherited.Source, config.DefaultWakeThreshold)
	}

	if _, err := service.Set(ctx, wakethreshold.Key("clanker"), 0.71); err != nil {
		t.Fatalf("Set: %v", err)
	}
	stored, _ := service.Get(wakethreshold.Key("clanker"))
	if stored.Value != 0.71 || stored.Source != settings.SourceOverride {
		t.Fatalf("stored clanker = %v from %s, want 0.71 from the override", stored.Value, stored.Source)
	}

	if _, err := service.Reset(ctx, wakethreshold.Key("clanker")); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	reset, _ := service.Get(wakethreshold.Key("clanker"))
	if reset.Value != 0.62 || reset.Source != settings.SourceConfig {
		t.Fatalf("after Reset clanker = %v from %s, want the environment 0.62", reset.Value, reset.Source)
	}
}

// TestThresholdValidationRefusesBadValues covers both halves of the write
// check: the probability range and the configured allowlist.
func TestThresholdValidationRefusesBadValues(t *testing.T) {
	cfg := config.Wakeword{Models: []string{"clanker"}}
	service := openSettings(t, wakethreshold.Definitions(cfg)...)
	ctx := context.Background()

	for _, bad := range []float64{0, -0.1, 1.01, 2} {
		if _, err := service.Set(ctx, wakethreshold.Key("clanker"), bad); err == nil {
			t.Errorf("Set(%v) was accepted, want a range refusal", bad)
		}
	}
	// The upper bound is inclusive.
	if _, err := service.Set(ctx, wakethreshold.Key("clanker"), 1.0); err != nil {
		t.Fatalf("Set(1): %v", err)
	}

	if _, err := service.Set(ctx, wakethreshold.Key("hey_jarvis"), 0.5); !errors.Is(err, settings.ErrUnknownSetting) {
		t.Fatalf("Set for an unconfigured model = %v, want ErrUnknownSetting", err)
	}
}

// TestStopModelHasAThresholdAndOnlyTheReplySourceSendsIt includes the
// configured stop model so the Settings page can tune it, while the idle
// source never carries it.
func TestStopModelHasAThresholdAndOnlyTheReplySourceSendsIt(t *testing.T) {
	cfg := config.Wakeword{
		Models:     []string{"clanker"},
		Stop:       "stop",
		Thresholds: map[string]float64{"stop": 0.9},
	}
	service := openSettings(t, wakethreshold.Definitions(cfg)...)

	value, ok := service.Get(wakethreshold.Key("stop"))
	if !ok {
		t.Fatal("stop threshold is not defined while a stop model is configured")
	}
	if value.Value != 0.9 {
		t.Fatalf("stop = %v from %s, want the configured 0.9", value.Value, value.Source)
	}
	if _, ok := wakethreshold.NewSource(service, cfg).Map()["stop"]; ok {
		t.Error("the idle source sends a stop threshold")
	}
	if got := wakethreshold.NewReplySource(service, cfg).Map()["stop"]; got != 0.9 {
		t.Errorf("reply source stop threshold = %v, want 0.9", got)
	}
}

// TestSignatureTracksTheEffectiveMap keeps the reopen check honest: the
// signature changes with a stored write, is stable across reads, and is sorted
// so config order does not look like an edit.
func TestSignatureTracksTheEffectiveMap(t *testing.T) {
	cfg := config.Wakeword{
		Models:     []string{"okay_nabu", "clanker"},
		Thresholds: map[string]float64{"clanker": 0.6, "okay_nabu": 0.5},
	}
	service := openSettings(t, wakethreshold.Definitions(cfg)...)
	source := wakethreshold.NewSource(service, cfg)
	ctx := context.Background()

	before := source.Signature()
	if got := source.Signature(); got != before {
		t.Fatalf("signature changed between reads: %q then %q", before, got)
	}
	if _, err := service.Set(ctx, wakethreshold.Key("clanker"), 0.66); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if after := source.Signature(); after == before {
		t.Fatalf("signature stayed %q after a stored write", before)
	}
	thresholds := source.Map()
	if thresholds["clanker"] != 0.66 || thresholds["okay_nabu"] != 0.5 {
		t.Fatalf("map = %v, want clanker 0.66 and okay_nabu 0.5", thresholds)
	}
}
