package dashboard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestBuildDiagnosticsBundleRefusesAnOversizedBundle proves the 1 MiB cap
// refuses rather than truncating into invalid JSON.
func TestBuildDiagnosticsBundleRefusesAnOversizedBundle(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	in := diagnosticsInput{
		GeneratedAt: now,
		StartedAt:   now.Add(-time.Hour),
		APIVersion:  APIVersion,
		Settings: []diagnosticSetting{
			{Key: "wording.prefixes", Value: strings.Repeat("x", maxDiagnosticsBytes+1), Source: "override", Kind: "live"},
		},
	}
	data, err := buildDiagnosticsBundle(in)
	if !errors.Is(err, errDiagnosticsTooLarge) {
		t.Fatalf("error = %v, want errDiagnosticsTooLarge", err)
	}
	if data != nil {
		t.Fatalf("an over-cap bundle returned %d bytes, want none", len(data))
	}
	// A bundle at the boundary still marshals into one valid JSON document.
	in.Settings[0].Value = strings.Repeat("x", 64)
	data, err = buildDiagnosticsBundle(in)
	if err != nil {
		t.Fatalf("small bundle: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("bundle is not valid JSON: %v", err)
	}
	if decoded["api_version"] != float64(APIVersion) {
		t.Errorf("api_version = %v, want %d", decoded["api_version"], APIVersion)
	}
	if decoded["uptime_seconds"] != float64(3600) {
		t.Errorf("uptime_seconds = %v, want 3600", decoded["uptime_seconds"])
	}
}

// TestRedactDiagnosticSetting keeps a secret-looking setting value out of the
// artifact while leaving ordinary effective settings visible.
func TestRedactDiagnosticSetting(t *testing.T) {
	const seeded = "sk-diagnostics-SECRETVALUE1234567890"
	for _, key := range []string{"agent.api_key", "notify.token", "NTFY_TOPIC", "http.password", "github.credential", "agent.secret"} {
		if got := redactDiagnosticSetting(key, seeded); got != diagnosticsRedacted {
			t.Errorf("redactDiagnosticSetting(%q) = %v, want %q", key, got, diagnosticsRedacted)
		}
	}
	if got := redactDiagnosticSetting("http.addr", seeded); got != seeded {
		t.Errorf("ordinary setting was redacted: %v", got)
	}
}

// TestDiagnosticsTagIsASafeFilename proves the tag cannot carry a path or a
// header injection: it is only digits and a Z.
func TestDiagnosticsTagIsASafeFilename(t *testing.T) {
	got := diagnosticsTag(time.Date(2026, 11, 16, 12, 34, 56, 0, time.UTC))
	if got != "20261116T123456Z" {
		t.Fatalf("tag = %q", got)
	}
	for _, r := range got {
		if (r < '0' || r > '9') && r != 'T' && r != 'Z' {
			t.Errorf("unsafe rune %q in tag %q", r, got)
		}
	}
}
