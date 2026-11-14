package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVoiceSamplesResolveUnderTheDataDirectory(t *testing.T) {
	cfg := Default()
	if got, want := cfg.VoiceSamplesDir(), filepath.Join("data", "wake-samples"); got != want {
		t.Errorf("VoiceSamplesDir = %q, want %q", got, want)
	}
	if got, want := cfg.VoiceSamplesExportDir(), filepath.Join("data", "exports"); got != want {
		t.Errorf("VoiceSamplesExportDir = %q, want %q", got, want)
	}
	cfg.Data.Dir = "/app/data"
	cfg.VoiceSamples.Dir = ""
	cfg.VoiceSamples.ExportDir = ""
	if got := cfg.VoiceSamplesDir(); got != "/app/data/wake-samples" {
		t.Errorf("empty dir = %q", got)
	}
	if got := cfg.VoiceSamplesExportDir(); got != "/app/data/exports" {
		t.Errorf("empty export dir = %q", got)
	}
	cfg.VoiceSamples.Dir = "custom"
	cfg.VoiceSamples.ExportDir = "/exports"
	if got := cfg.VoiceSamplesDir(); got != "/app/data/custom" {
		t.Errorf("relative dir = %q", got)
	}
	if got := cfg.VoiceSamplesExportDir(); got != "/exports" {
		t.Errorf("absolute export dir = %q", got)
	}
}

func TestVoiceSamplesEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("data:\n  dir: /srv/data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_VOICE_SAMPLES_DIR", "/srv/samples")
	t.Setenv("ASSISTANT_VOICE_SAMPLES_EXPORT_DIR", "/srv/exports")
	t.Setenv("ASSISTANT_VOICE_SAMPLES_HOST_DIR", "/home/rbpi/clanka-bot/exports")
	t.Setenv("ASSISTANT_EXPORT_SSH_USER", "rbpi")
	t.Setenv("ASSISTANT_EXPORT_SSH_HOST", "pi")
	t.Setenv("ASSISTANT_EXPORT_DESKTOP", "desktop")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.VoiceSamples.Dir != "/srv/samples" || cfg.VoiceSamples.ExportDir != "/srv/exports" {
		t.Errorf("paths = %+v", cfg.VoiceSamples)
	}
	if cfg.VoiceSamples.HostDir != "/home/rbpi/clanka-bot/exports" || cfg.VoiceSamples.SSHUser != "rbpi" || cfg.VoiceSamples.SSHHost != "pi" || cfg.VoiceSamples.Desktop != "desktop" {
		t.Errorf("copy config = %+v", cfg.VoiceSamples)
	}
}

func TestVoiceSamplesRejectsANegativeMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("voice_samples:\n  max_takes: -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a negative max_takes")
	}
}
