package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadReadsFile(t *testing.T) {
	path := writeConfig(t, `
http:
  addr: ":9090"
debug:
  enabled: false
  addr: "127.0.0.1:9091"
tools:
  default_timeout: 750ms
control_phrases:
  "what time is it": time.now
embed:
  url: "http://embed-test:9000"
  model: "BAAI/bge-small-en-v1.5"
  timeout: 250ms
semantic:
  threshold: 0.75
  margin: 0.1
  joiners: ["and", "then"]
agent:
  request_prefixes: ["ask the agent", "hey agent"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("HTTP.Addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Debug.Enabled {
		t.Error("Debug.Enabled = true, want false")
	}
	if cfg.Debug.Addr != "127.0.0.1:9091" {
		t.Errorf("Debug.Addr = %q", cfg.Debug.Addr)
	}
	if cfg.Tools.DefaultTimeout.Duration() != 750*time.Millisecond {
		t.Errorf("DefaultTimeout = %s", cfg.Tools.DefaultTimeout.Duration())
	}
	if got := cfg.ControlPhrases["what time is it"]; got != "time.now" {
		t.Errorf("control phrase = %q", got)
	}
	if cfg.Embed.URL != "http://embed-test:9000" {
		t.Errorf("Embed.URL = %q", cfg.Embed.URL)
	}
	if cfg.Embed.Model != "BAAI/bge-small-en-v1.5" {
		t.Errorf("Embed.Model = %q", cfg.Embed.Model)
	}
	if cfg.Embed.Timeout.Duration() != 250*time.Millisecond {
		t.Errorf("Embed.Timeout = %s", cfg.Embed.Timeout.Duration())
	}
	if cfg.Semantic.Threshold != 0.75 {
		t.Errorf("Semantic.Threshold = %v", cfg.Semantic.Threshold)
	}
	if cfg.Semantic.Margin != 0.1 {
		t.Errorf("Semantic.Margin = %v", cfg.Semantic.Margin)
	}
	if len(cfg.Semantic.Joiners) != 2 || cfg.Semantic.Joiners[0] != "and" {
		t.Errorf("Semantic.Joiners = %v", cfg.Semantic.Joiners)
	}
	if len(cfg.Agent.RequestPrefixes) != 2 || cfg.Agent.RequestPrefixes[1] != "hey agent" {
		t.Errorf("Agent.RequestPrefixes = %v", cfg.Agent.RequestPrefixes)
	}
}

func TestLoadAppliesDefaultsForOmittedFields(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if !cfg.Debug.Enabled {
		t.Error("Debug.Enabled = false, want default true")
	}
	if cfg.Debug.Addr != "127.0.0.1:8081" {
		t.Errorf("Debug.Addr = %q, want 127.0.0.1:8081", cfg.Debug.Addr)
	}
	if cfg.Tools.DefaultTimeout.Duration() != 2*time.Second {
		t.Errorf("DefaultTimeout = %s, want 2s", cfg.Tools.DefaultTimeout.Duration())
	}
	if cfg.Embed.URL != "http://embed:8085" {
		t.Errorf("Embed.URL = %q, want http://embed:8085", cfg.Embed.URL)
	}
	if cfg.Embed.Model != "sentence-transformers/all-MiniLM-L6-v2" {
		t.Errorf("Embed.Model = %q, want all-MiniLM-L6-v2", cfg.Embed.Model)
	}
	if cfg.Embed.Timeout.Duration() != 150*time.Millisecond {
		t.Errorf("Embed.Timeout = %s, want 150ms", cfg.Embed.Timeout.Duration())
	}
	if cfg.Semantic.Threshold != 0.70 {
		t.Errorf("Semantic.Threshold = %v, want 0.70", cfg.Semantic.Threshold)
	}
	if cfg.Semantic.Margin != 0.08 {
		t.Errorf("Semantic.Margin = %v, want 0.08", cfg.Semantic.Margin)
	}
	if len(cfg.Semantic.Joiners) == 0 {
		t.Error("Semantic.Joiners is empty")
	}
	if len(cfg.Agent.RequestPrefixes) == 0 {
		t.Error("Agent.RequestPrefixes is empty")
	}
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("ASSISTANT_HTTP_ADDR", ":7070")
	t.Setenv("ASSISTANT_DEBUG_ADDR", ":7071")
	t.Setenv("ASSISTANT_DEBUG_ENABLED", "false")
	t.Setenv("ASSISTANT_EMBED_URL", "http://embed-env:9000")
	t.Setenv("ASSISTANT_EMBED_MODEL", "BAAI/bge-small-en-v1.5")
	t.Setenv("ASSISTANT_SEMANTIC_THRESHOLD", "0.8")
	t.Setenv("ASSISTANT_SEMANTIC_MARGIN", "0.12")

	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":7070" {
		t.Errorf("HTTP.Addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Debug.Addr != ":7071" {
		t.Errorf("Debug.Addr = %q", cfg.Debug.Addr)
	}
	if cfg.Debug.Enabled {
		t.Error("Debug.Enabled = true, want false")
	}
	if cfg.Embed.URL != "http://embed-env:9000" {
		t.Errorf("Embed.URL = %q", cfg.Embed.URL)
	}
	if cfg.Embed.Model != "BAAI/bge-small-en-v1.5" {
		t.Errorf("Embed.Model = %q", cfg.Embed.Model)
	}
	if cfg.Semantic.Threshold != 0.8 {
		t.Errorf("Semantic.Threshold = %v", cfg.Semantic.Threshold)
	}
	if cfg.Semantic.Margin != 0.12 {
		t.Errorf("Semantic.Margin = %v", cfg.Semantic.Margin)
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bad duration", "tools:\n  default_timeout: soon\n"},
		{"negative duration", "tools:\n  default_timeout: -1s\n"},
		{"empty http addr", "http:\n  addr: \"\"\n"},
		{"zero embed timeout", "embed:\n  timeout: 0s\n"},
		{"empty embed url", "embed:\n  url: \"\"\n"},
		{"empty embed model", "embed:\n  model: \"\"\n"},
		{"zero threshold", "semantic:\n  threshold: 0\n"},
		{"threshold above one", "semantic:\n  threshold: 1.5\n"},
		{"negative margin", "semantic:\n  margin: -0.1\n"},
		{"margin of one", "semantic:\n  margin: 1.0\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, tt.body)); err == nil {
				t.Fatal("Load succeeded, want error")
			}
		})
	}
}

func TestLoadRejectsInvalidDebugEnabledEnv(t *testing.T) {
	t.Setenv("ASSISTANT_DEBUG_ENABLED", "maybe")
	if _, err := config.Load(writeConfig(t, "{}\n")); err == nil {
		t.Fatal("Load succeeded, want error")
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("Load succeeded, want error")
	}
}

func TestLoadReadsSpeechSettings(t *testing.T) {
	path := writeConfig(t, `
tts:
  addr: "tts:10200"
  voice: "alba"
  timeout: 12s
playback:
  binary: "pacat"
  sink: "echo_dot"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TTS.Addr != "tts:10200" || cfg.TTS.Voice != "alba" {
		t.Errorf("tts = %+v", cfg.TTS)
	}
	if cfg.TTS.Timeout.Duration() != 12*time.Second {
		t.Errorf("tts timeout = %s", cfg.TTS.Timeout.Duration())
	}
	if cfg.Playback.Sink != "echo_dot" || cfg.Playback.Binary != "pacat" {
		t.Errorf("playback = %+v", cfg.Playback)
	}
}

func TestLoadAppliesSpeechEnvironmentOverrides(t *testing.T) {
	t.Setenv("ASSISTANT_TTS_ADDR", "127.0.0.1:10200")
	t.Setenv("ASSISTANT_TTS_VOICE", "marius")
	t.Setenv("ASSISTANT_PULSE_SINK", "alsa_output.test")

	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TTS.Addr != "127.0.0.1:10200" || cfg.TTS.Voice != "marius" {
		t.Errorf("tts = %+v", cfg.TTS)
	}
	if cfg.Playback.Sink != "alsa_output.test" {
		t.Errorf("playback sink = %q", cfg.Playback.Sink)
	}
}

func TestLoadRejectsNonPositiveSpeechTimeout(t *testing.T) {
	if _, err := config.Load(writeConfig(t, "tts:\n  addr: \"tts:10200\"\n  timeout: 0s\n")); err == nil {
		t.Fatal("Load succeeded, want error")
	}
}
