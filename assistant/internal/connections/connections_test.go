package connections_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/connections"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

func openService(t *testing.T, defaults, cfg config.Config) *settings.Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	svc, err := settings.New(context.Background(), st, connections.Definitions(defaults, cfg)...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc
}

func TestEveryConnectionSettingIsRestartRequired(t *testing.T) {
	cfg := config.Default()
	for _, def := range connections.Definitions(config.Default(), cfg) {
		if def.Kind != settings.RestartRequired {
			t.Errorf("%s kind = %q, want restart_required", def.Key, def.Kind)
		}
	}
}

func TestApplyResolvesStoredOverridesAtStartupAndResetRestoresConfig(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	svc := openService(t, config.Default(), cfg)

	if _, err := svc.Set(ctx, connections.HTTPAddrKey, "127.0.0.1:9090"); err != nil {
		t.Fatalf("Set http.addr: %v", err)
	}
	if _, err := svc.Set(ctx, connections.CaptureSourceKey, "alsa_input.usb"); err != nil {
		t.Fatalf("Set capture.source: %v", err)
	}

	applied := cfg
	connections.Apply(&applied, svc)
	if applied.HTTP.Addr != "127.0.0.1:9090" {
		t.Fatalf("http.addr = %q, want the stored override", applied.HTTP.Addr)
	}
	if applied.Capture.Source != "alsa_input.usb" {
		t.Fatalf("capture.source = %q, want the stored override", applied.Capture.Source)
	}
	if applied.Embed.URL != cfg.Embed.URL {
		t.Fatalf("embed.url = %q, want the config value", applied.Embed.URL)
	}

	if _, err := svc.Reset(ctx, connections.HTTPAddrKey); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	applied = cfg
	connections.Apply(&applied, svc)
	if applied.HTTP.Addr != cfg.HTTP.Addr {
		t.Fatalf("http.addr after reset = %q, want %q", applied.HTTP.Addr, cfg.HTTP.Addr)
	}
}

func TestDashboardAddressOverrideCannotWidenTheListener(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Dashboard.Owner = "owner@example.com"
	svc := openService(t, config.Default(), cfg)

	if _, err := svc.Set(ctx, connections.DashboardAddrKey, "192.168.1.5:8082"); err == nil {
		t.Fatal("Set accepted a non-loopback dashboard address")
	}
	if _, err := svc.Set(ctx, connections.DashboardAddrKey, "127.0.0.1:9099"); err != nil {
		t.Fatalf("Set loopback address: %v", err)
	}
}

func TestDebugAddressCannotBeEmptyWhileEnabled(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Debug.Enabled = true
	svc := openService(t, config.Default(), cfg)

	if _, err := svc.Set(ctx, connections.DebugAddrKey, ""); err == nil {
		t.Fatal("Set accepted an empty debug address while the entry is enabled")
	}
}
