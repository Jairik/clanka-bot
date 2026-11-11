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
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("ASSISTANT_HTTP_ADDR", ":7070")
	t.Setenv("ASSISTANT_DEBUG_ADDR", ":7071")
	t.Setenv("ASSISTANT_DEBUG_ENABLED", "false")

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
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bad duration", "tools:\n  default_timeout: soon\n"},
		{"negative duration", "tools:\n  default_timeout: -1s\n"},
		{"empty http addr", "http:\n  addr: \"\"\n"},
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
