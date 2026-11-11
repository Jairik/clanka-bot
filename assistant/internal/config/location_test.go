package config_test

import (
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
)

func TestLocationDefaults(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "{}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Location.ProviderURL != "https://ipwho.is/?fields=success,city,region,timezone" {
		t.Errorf("ProviderURL = %q", cfg.Location.ProviderURL)
	}
	if cfg.Location.RefreshInterval.Duration() != 24*time.Hour {
		t.Errorf("RefreshInterval = %s, want 24h", cfg.Location.RefreshInterval.Duration())
	}
	if cfg.Location.Timeout.Duration() != 5*time.Second {
		t.Errorf("Timeout = %s, want 5s", cfg.Location.Timeout.Duration())
	}
	if cfg.Location.Fallback.City != "" || cfg.Location.Fallback.Timezone != "" {
		t.Errorf("Fallback = %+v, want empty", cfg.Location.Fallback)
	}
}

func TestLoadReadsLocationFallback(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
location:
  provider_url: "https://geo.example.test/"
  refresh_interval: 6h
  timeout: 2s
  fallback:
    city: "Paris"
    region: "Ile-de-France"
    timezone: "Europe/Paris"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Location.ProviderURL != "https://geo.example.test/" {
		t.Errorf("ProviderURL = %q", cfg.Location.ProviderURL)
	}
	if cfg.Location.RefreshInterval.Duration() != 6*time.Hour {
		t.Errorf("RefreshInterval = %s", cfg.Location.RefreshInterval.Duration())
	}
	if cfg.Location.Timeout.Duration() != 2*time.Second {
		t.Errorf("Timeout = %s", cfg.Location.Timeout.Duration())
	}
	if cfg.Location.Fallback.City != "Paris" ||
		cfg.Location.Fallback.Region != "Ile-de-France" ||
		cfg.Location.Fallback.Timezone != "Europe/Paris" {
		t.Errorf("Fallback = %+v", cfg.Location.Fallback)
	}
}

func TestLoadRejectsInvalidLocationRefresh(t *testing.T) {
	if _, err := config.Load(writeConfig(t, "location:\n  refresh_interval: 0s\n")); err == nil {
		t.Fatal("Load succeeded, want a refresh interval error")
	}
}
