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
	path := filepath.Join(t.TempDir(), "soloist.status")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}
	return path
}

func readStatus(t *testing.T, body string) spotify.PairingStatus {
	t.Helper()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return spotify.PairingReader{
		Path:       writeStatus(t, body),
		StaleAfter: 45 * time.Second,
		Now:        func() time.Time { return now },
	}.Status()
}

func TestPairingStatusReportsPairedAndUnpaired(t *testing.T) {
	status := readStatus(t, "version=2\nstate=paired\nsession=active\ndevice=Clanka\nexpires_days=89\nupdated=2026-11-12T11:59:50Z\n")
	if !status.Configured || !status.Available || !status.Paired {
		t.Fatalf("status = %+v, want paired and available", status)
	}
	if status.State != spotify.PairingPaired {
		t.Fatalf("state = %q", status.State)
	}
	if status.Session != "active" || !status.ExpiryKnown || status.ExpiresDays != 89 || status.Expired {
		t.Fatalf("status = %+v, want active, 89 days left and not expired", status)
	}
	if !strings.Contains(status.Detail, "Paired") || !strings.Contains(status.Detail, "Clanka") {
		t.Fatalf("detail = %q", status.Detail)
	}
	if !strings.Contains(status.Detail, "active Spotify device") {
		t.Fatalf("detail = %q, want the active device", status.Detail)
	}

	status = readStatus(t, "version=2\nstate=unpaired\nsession=idle\ndevice=Clanka\nexpires_days=89\nupdated=2026-11-12T11:59:50Z\n")
	if !status.Available || status.Paired || status.State != spotify.PairingNotPaired {
		t.Fatalf("status = %+v, want unpaired and available", status)
	}
	if !strings.Contains(status.Detail, "Spotify → Devices") {
		t.Fatalf("detail = %q", status.Detail)
	}
}

func TestPairingStatusWarnsAndFailsOnBuildExpiry(t *testing.T) {
	status := readStatus(t, "version=2\nstate=paired\nsession=idle\ndevice=Clanka\nexpires_days=9\nupdated=2026-11-12T11:59:50Z\n")
	if !status.Paired || status.Expired || status.State != spotify.PairingPaired {
		t.Fatalf("status = %+v, want paired with a warning", status)
	}
	if !strings.Contains(status.Detail, "expires in 9 days") || !strings.Contains(status.Detail, "docs/deploy.md") {
		t.Fatalf("detail = %q, want an actionable expiry warning", status.Detail)
	}

	status = readStatus(t, "version=2\nstate=paired\nsession=idle\ndevice=Clanka\nexpires_days=0\nupdated=2026-11-12T11:59:50Z\n")
	if !strings.Contains(status.Detail, "expires today") {
		t.Fatalf("detail = %q, want today's expiry called out", status.Detail)
	}

	status = readStatus(t, "version=2\nstate=paired\nsession=idle\ndevice=Clanka\nexpires_days=-2\nupdated=2026-11-12T11:59:50Z\n")
	if !status.Available || !status.Expired || status.State != spotify.PairingExpired {
		t.Fatalf("status = %+v, want an expired build", status)
	}
	if !status.Paired {
		t.Fatal("an expired build still reports the cached credentials as paired")
	}
	if !strings.Contains(status.Detail, "expired") || !strings.Contains(status.Detail, "Update") {
		t.Fatalf("detail = %q", status.Detail)
	}
}

func TestPairingStatusKeepsUnknownExpirySeparateFromUnavailable(t *testing.T) {
	status := readStatus(t, "version=2\nstate=paired\nsession=idle\ndevice=Clanka\nexpires_days=unknown\nupdated=2026-11-12T11:59:50Z\n")
	if !status.Available || !status.Paired || status.ExpiryKnown || status.Expired {
		t.Fatalf("status = %+v, want paired with unknown expiry", status)
	}
}

func TestPairingStatusIsUnavailableWhenMissingStaleOrMalformed(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(t.TempDir(), "absent")},
		{"stale", writeStatus(t, "version=2\nstate=paired\nupdated=2026-11-12T11:00:00Z\n")},
		{"malformed state", writeStatus(t, "version=2\nstate=?\nupdated=2026-11-12T11:59:50Z\n")},
		{"old version", writeStatus(t, "version=1\nstate=paired\nupdated=2026-11-12T11:59:50Z\n")},
		{"no version", writeStatus(t, "state=paired\nupdated=2026-11-12T11:59:50Z\n")},
		{"no timestamp", writeStatus(t, "version=2\nstate=paired\n")},
		{"future timestamp", writeStatus(t, "version=2\nstate=paired\nupdated=2026-11-12T13:00:00Z\n")},
		{"bad expiry", writeStatus(t, "version=2\nstate=paired\nexpires_days=soon\nupdated=2026-11-12T11:59:50Z\n")},
		{"control socket down", writeStatus(t, "version=2\nstate=unavailable\nsession=unknown\nupdated=2026-11-12T11:59:50Z\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
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
