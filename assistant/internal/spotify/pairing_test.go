package spotify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
)

func writeStatus(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "librespot.status")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}
	return path
}

func TestPairingStatusReportsPairedAndUnpaired(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	path := writeStatus(t, "version=1\nstate=paired\nsession=connected\ndevice=Clanka\nupdated=2026-11-12T11:59:50Z\n")
	reader := spotify.PairingReader{Path: path, StaleAfter: 45 * time.Second, Now: func() time.Time { return now }}
	status := reader.Status()
	if !status.Configured || !status.Available || !status.Paired {
		t.Fatalf("status = %+v, want paired and available", status)
	}
	if status.Session != "connected" {
		t.Fatalf("session = %q", status.Session)
	}
	if !strings.Contains(status.Detail, "Paired") || !strings.Contains(status.Detail, "Clanka") {
		t.Fatalf("detail = %q", status.Detail)
	}

	path = writeStatus(t, "version=1\nstate=unpaired\nsession=unknown\ndevice=Clanka\nupdated=2026-11-12T11:59:50Z\n")
	status = spotify.PairingReader{Path: path, StaleAfter: 45 * time.Second, Now: func() time.Time { return now }}.Status()
	if !status.Available || status.Paired {
		t.Fatalf("status = %+v, want unpaired and available", status)
	}
	if !strings.Contains(status.Detail, "Spotify → Devices") {
		t.Fatalf("detail = %q", status.Detail)
	}
}

func TestPairingStatusIsUnavailableWhenMissingStaleOrMalformed(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(t.TempDir(), "absent")},
		{"stale", writeStatus(t, "version=1\nstate=paired\nupdated=2026-11-12T11:00:00Z\n")},
		{"malformed", writeStatus(t, "version=1\nstate=?\nupdated=2026-11-12T11:59:50Z\n")},
		{"no version", writeStatus(t, "state=paired\nupdated=2026-11-12T11:59:50Z\n")},
		{"no timestamp", writeStatus(t, "version=1\nstate=paired\n")},
		{"future timestamp", writeStatus(t, "version=1\nstate=paired\nupdated=2026-11-12T13:00:00Z\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := spotify.PairingReader{Path: tt.path, StaleAfter: 45 * time.Second, Now: func() time.Time { return now }}.Status()
			if status.Available || status.Paired {
				t.Fatalf("status = %+v, want unavailable", status)
			}
			if status.State != spotify.PairingUnavailable {
				t.Fatalf("state = %q", status.State)
			}
			if status.Detail == "" {
				t.Fatal("unavailable status has no detail")
			}
		})
	}
}

func TestPairingStatusIsNotConfiguredWithoutAPath(t *testing.T) {
	status := spotify.PairingReader{}.Status()
	if status.Configured || status.Available {
		t.Fatalf("status = %+v, want not configured", status)
	}
	if status.State != spotify.PairingNotConfigured {
		t.Fatalf("state = %q", status.State)
	}
}
