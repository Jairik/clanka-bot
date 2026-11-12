package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

func TestVoiceDefaultsAndDirectoryResolution(t *testing.T) {
	cfg := config.Default()
	if cfg.Voice.TTSDir != "/voices" || cfg.Voice.Rate != 1.0 || cfg.Voice.Dir != "voices" {
		t.Fatalf("voice defaults = %+v", cfg.Voice)
	}
	cfg.Data.Dir = "/srv/clanka"
	if got := cfg.VoiceDir(); got != filepath.Join("/srv/clanka", "voices") {
		t.Fatalf("VoiceDir = %q", got)
	}
	cfg.Voice.Dir = "custom/voices"
	if got := cfg.VoiceDir(); got != filepath.Join("/srv/clanka", "custom/voices") {
		t.Fatalf("relative VoiceDir = %q", got)
	}
	cfg.Voice.Dir = "/mnt/voice-volume"
	if got := cfg.VoiceDir(); got != "/mnt/voice-volume" {
		t.Fatalf("absolute VoiceDir = %q", got)
	}
	cfg.Voice.Dir = ""
	if got := cfg.VoiceDir(); got != filepath.Join("/srv/clanka", "voices") {
		t.Fatalf("empty VoiceDir = %q", got)
	}
}

func TestLoadReadsTheVoiceBlockAndEnvironment(t *testing.T) {
	path := writeConfig(t, `
voice:
  dir: "/mnt/voice-volume"
  tts_dir: "/mounted/voices"
  rate: 1.2
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Voice.Dir != "/mnt/voice-volume" || cfg.Voice.TTSDir != "/mounted/voices" || cfg.Voice.Rate != 1.2 {
		t.Fatalf("voice = %+v", cfg.Voice)
	}

	t.Setenv("ASSISTANT_VOICE_DIR", "env-voices")
	t.Setenv("ASSISTANT_VOICE_TTS_DIR", "/env-voices")
	t.Setenv("ASSISTANT_VOICE_RATE", "0.8")
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Voice.Dir != "env-voices" || cfg.Voice.TTSDir != "/env-voices" || cfg.Voice.Rate != 0.8 {
		t.Fatalf("env voice = %+v", cfg.Voice)
	}
}

func TestLoadRejectsInvalidVoiceSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "rate above the range", body: "voice:\n  rate: 2.5\n"},
		{name: "rate below the range", body: "voice:\n  rate: 0.1\n"},
		{name: "tts dir is relative", body: "tts:\n  addr: \"tts:10200\"\nvoice:\n  tts_dir: \"voices\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, tc.body)); err == nil {
				t.Fatal("Load accepted an invalid voice setting")
			}
		})
	}
}

func TestLoadAcceptsARelativeTTSDirWithoutTTS(t *testing.T) {
	// voice.tts_dir only has to be absolute when a TTS service is configured;
	// a text-only process never sends a voice path.
	path := writeConfig(t, "tts:\n  addr: \"\"\nvoice:\n  tts_dir: \"local\"\n")
	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadRejectsANonNumericVoiceRateEnv(t *testing.T) {
	t.Setenv("ASSISTANT_VOICE_RATE", "fast")
	_, err := config.Load(writeConfig(t, "voice:\n  rate: 1.0\n"))
	if err == nil || !strings.Contains(err.Error(), "ASSISTANT_VOICE_RATE") {
		t.Fatalf("error = %v", err)
	}
}
