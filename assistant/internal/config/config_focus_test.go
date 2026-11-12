package config_test

import (
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

// TestControlPhrasesAcceptIntentsAndTools is the table contract at the config
// layer: a bare value is one of the fixed Focus intents, a dotted value is a
// Tool name that the pipeline validates against the registry.
func TestControlPhrasesAcceptIntentsAndTools(t *testing.T) {
	path := writeConfig(t, `
control_phrases:
  "stop": stop
  "pause": pause
  "cancel": cancel
  "louder": audio.volume_set
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ControlPhrases["stop"]; got != "stop" {
		t.Fatalf("stop phrase = %q, want the intent", got)
	}
	if got := cfg.ControlPhrases["louder"]; got != "audio.volume_set" {
		t.Fatalf("louder phrase = %q, want the Tool", got)
	}
}

// TestControlPhrasesRejectUnknownIntents stops a typo from becoming a Control
// phrase that silently never fires.
func TestControlPhrasesRejectUnknownIntents(t *testing.T) {
	path := writeConfig(t, `
control_phrases:
  "stop": stopp
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load accepted an unknown intent")
	}
	if !strings.Contains(err.Error(), "unknown intent") || !strings.Contains(err.Error(), "stopp") {
		t.Fatalf("error = %q, want it to name the unknown intent", err)
	}
}

// TestControlPhrasesRejectAnEmptyTarget keeps a blank table value from
// disabling a phrase without saying so.
func TestControlPhrasesRejectAnEmptyTarget(t *testing.T) {
	path := writeConfig(t, `
control_phrases:
  "stop": ""
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load accepted an empty Control phrase target")
	}
}
