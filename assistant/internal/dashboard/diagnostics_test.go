package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
)

const (
	diagOpenRouterKey = "OPENROUTER_API_KEY"
	diagGitHubToken   = "GITHUB_TOKEN"
	diagNtfyTopic     = "NTFY_TOPIC"
	diagOpenRouterVal = "sk-or-diagnostics-SECRETVALUE12345"
	diagGitHubVal     = "ghp_diagnosticsSECRETVALUE67890"
	diagNtfyVal       = "diagnostics-private-topic-SECRETVALUE"
	diagOwnerText     = "OWNER_REQUEST_TEXT_INCLUDED"
	diagLocalOnlyText = "LOCAL_ONLY_TOOL_OUTPUT_TEXT"
)

// diagnosticsFixture wires the real services the bundle reads, so the tests
// prove the route cannot leak a stored value, a topic or a local-only result.
type diagnosticsFixture struct {
	t       *testing.T
	handler *dashboard.Handler
}

func newDiagnosticsFixture(t *testing.T, pin string) *diagnosticsFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "diagnostics.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc, err := secrets.New(ctx, st.DB(), []string{diagOpenRouterKey, diagGitHubToken, diagNtfyTopic})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	for name, value := range map[string]string{
		diagOpenRouterKey: diagOpenRouterVal,
		diagGitHubToken:   diagGitHubVal,
		diagNtfyTopic:     diagNtfyVal,
	} {
		if _, err := svc.Set(ctx, name, value); err != nil {
			t.Fatalf("secrets.Set(%s): %v", name, err)
		}
	}

	registry := tool.NewRegistry(time.Second)
	if err := tool.Add(registry, tool.Tool[struct{}, string]{
		Name:        "time.now",
		Description: "test Tool",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler:     func(context.Context, struct{}) (string, error) { return "now", nil },
	}); err != nil {
		t.Fatalf("tool.Add: %v", err)
	}
	defs, err := toolpolicy.Definitions(registry, toolpolicy.Config{})
	if err != nil {
		t.Fatalf("toolpolicy.Definitions: %v", err)
	}
	settingsSvc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	policy, err := toolpolicy.New(settingsSvc, registry)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}

	act := activity.New(100)
	act.Observe(latency.Completed{
		ID:         "req-local",
		Source:     latency.SourceVoice,
		Path:       "fast_path",
		Transcript: diagLocalOnlyText,
		Outcome:    latency.OutcomeOK,
		Detail: &latency.Detail{
			Reply:     diagLocalOnlyText,
			LocalOnly: true,
			Calls: []latency.ToolCallDetail{
				{Name: "time.now", LocalOnly: true, Result: diagLocalOnlyText},
			},
		},
	})
	act.Observe(latency.Completed{
		ID:         "req-owner",
		Source:     latency.SourceDashboard,
		Path:       "agent_path",
		Transcript: diagOwnerText,
		Outcome:    latency.OutcomeOK,
		Detail: &latency.Detail{
			Reply: diagOwnerText,
			Calls: []latency.ToolCallDetail{
				{Name: "weather.current", Result: diagOwnerText},
			},
		},
	})

	fixed := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	handler := newHandler(t, dashboard.Options{
		Secrets:    svc,
		Settings:   settingsSvc,
		ToolPolicy: policy,
		Activity:   act,
		UIVersion:  pin,
		Now:        func() time.Time { return fixed },
	})
	return &diagnosticsFixture{t: t, handler: handler}
}

// bundleRow is the subset of the artifact the assertions read.
type bundleRow struct {
	GeneratedAt   string `json:"generated_at"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	APIVersion    int    `json:"api_version"`
	UI            struct {
		Pin             string `json:"pin"`
		ReportedVersion string `json:"reported_version"`
	} `json:"ui"`
	Settings []struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	} `json:"settings"`
	Tools []struct {
		Name       string `json:"name"`
		Enabled    bool   `json:"enabled"`
		Permission string `json:"permission"`
		LocalOnly  bool   `json:"local_only"`
	} `json:"tools"`
	Secrets []struct {
		Name   string `json:"name"`
		Set    bool   `json:"set"`
		Source string `json:"source"`
	} `json:"secrets"`
	Activity []struct {
		ID         string `json:"id"`
		Transcript string `json:"transcript"`
		Reply      string `json:"reply"`
		LocalOnly  bool   `json:"local_only"`
		Calls      []struct {
			Name      string `json:"name"`
			LocalOnly bool   `json:"local_only"`
			Result    string `json:"result"`
		} `json:"calls"`
	} `json:"activity"`
	ActivityNotice string `json:"activity_notice"`
}

func fetchBundle(t *testing.T, handler http.Handler, target string) (*httptest.ResponseRecorder, bundleRow) {
	t.Helper()
	rec := serve(handler, request(http.MethodGet, target, owner))
	var row bundleRow
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
			t.Fatalf("bundle is not valid JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, row
}

func TestDiagnosticsBundleContainsExpectedEntries(t *testing.T) {
	f := newDiagnosticsFixture(t, "pin-sha")
	rec, row := fetchBundle(t, f.handler, "/api/ui/diagnostics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if row.APIVersion != dashboard.APIVersion {
		t.Errorf("api_version = %d, want %d", row.APIVersion, dashboard.APIVersion)
	}
	if row.UI.Pin != "pin-sha" || row.UI.ReportedVersion != "pin-sha" {
		t.Errorf("ui = %+v, want the pin as both pin and reported version", row.UI)
	}
	if row.GeneratedAt == "" || row.UptimeSeconds < 0 {
		t.Errorf("missing generated_at or bad uptime: %+v", row)
	}
	if len(row.Settings) == 0 {
		t.Error("bundle has no effective settings")
	}
	if len(row.Tools) != 1 || row.Tools[0].Name != "time.now" || !row.Tools[0].LocalOnly || row.Tools[0].Permission != "read" {
		t.Errorf("tool policy entries = %+v", row.Tools)
	}
	if len(row.Secrets) != 3 {
		t.Fatalf("secret metadata entries = %d, want 3: %+v", len(row.Secrets), row.Secrets)
	}
	for _, secret := range row.Secrets {
		if !secret.Set || secret.Source == "" {
			t.Errorf("secret metadata is incomplete: %+v", secret)
		}
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "clanka-diagnostics-20261116T120000Z.json") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestDiagnosticsBundleNeverContainsSecretsOrTopics(t *testing.T) {
	f := newDiagnosticsFixture(t, "pin-sha")
	rec, row := fetchBundle(t, f.handler, "/api/ui/diagnostics?requests=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{diagOpenRouterVal, diagGitHubVal, diagNtfyVal} {
		if strings.Contains(body, forbidden) {
			t.Errorf("bundle contains a secret value or topic %q", forbidden)
		}
		if len(forbidden) >= 4 && strings.Contains(body, forbidden[len(forbidden)-4:]) {
			t.Errorf("bundle contains a last4 hint of %q", forbidden)
		}
	}
	for _, secret := range row.Secrets {
		raw, _ := json.Marshal(secret)
		if strings.Contains(string(raw), "last4") || strings.Contains(string(raw), "value") {
			t.Errorf("secret metadata entry carries more than set/source: %s", raw)
		}
	}
	if len(row.Activity) != 2 {
		t.Fatalf("activity entries = %d, want 2", len(row.Activity))
	}
	local, owner := -1, -1
	for i := range row.Activity {
		switch row.Activity[i].ID {
		case "req-local":
			local = i
		case "req-owner":
			owner = i
		}
	}
	if local < 0 || owner < 0 {
		t.Fatalf("activity missing a seeded entry: %+v", row.Activity)
	}
	if strings.Contains(body, diagLocalOnlyText) {
		t.Error("bundle leaked a local-only Tool result")
	}
	l := row.Activity[local]
	if l.Transcript != "" || l.Reply != "" || len(l.Calls) != 1 || l.Calls[0].Result == diagLocalOnlyText {
		t.Errorf("local-only entry kept its output: %+v", l)
	}
	o := row.Activity[owner]
	if !strings.Contains(body, diagOwnerText) || o.Transcript != diagOwnerText || o.Reply != diagOwnerText {
		t.Errorf("ordinary opt-in request text was not included: %+v", o)
	}
	if row.ActivityNotice == "" {
		t.Error("bundle with requests has no privacy notice")
	}
}

func TestDiagnosticsRequestsAreOptIn(t *testing.T) {
	f := newDiagnosticsFixture(t, "pin-sha")
	_, plain := fetchBundle(t, f.handler, "/api/ui/diagnostics")
	if len(plain.Activity) != 0 {
		t.Fatalf("default bundle has %d activity entries, want 0", len(plain.Activity))
	}
	_, withRequests := fetchBundle(t, f.handler, "/api/ui/diagnostics?requests=1")
	if len(withRequests.Activity) != 2 {
		t.Fatalf("requests=1 bundle has %d activity entries, want 2", len(withRequests.Activity))
	}
}

func TestDiagnosticsRouteRefusesUntrustedSourceAndHost(t *testing.T) {
	f := newDiagnosticsFixture(t, "pin-sha")

	untrusted := request(http.MethodGet, "/api/ui/diagnostics", owner)
	untrusted.RemoteAddr = "203.0.113.9:1234"
	if rec := serve(f.handler, untrusted); rec.Code != http.StatusForbidden {
		t.Errorf("untrusted source status = %d, want 403", rec.Code)
	}
	hostile := request(http.MethodGet, "/api/ui/diagnostics", owner)
	hostile.Host = "attacker.example"
	if rec := serve(f.handler, hostile); rec.Code != http.StatusForbidden {
		t.Errorf("hostile Host status = %d, want 403", rec.Code)
	}
	missing := request(http.MethodGet, "/api/ui/diagnostics", "")
	if rec := serve(f.handler, missing); rec.Code != http.StatusForbidden {
		t.Errorf("missing identity status = %d, want 403", rec.Code)
	}
	wrong := request(http.MethodGet, "/api/ui/diagnostics", "mallory@example.com")
	if rec := serve(f.handler, wrong); rec.Code != http.StatusForbidden {
		t.Errorf("wrong identity status = %d, want 403", rec.Code)
	}
	post := request(http.MethodPost, "/api/ui/diagnostics", owner)
	if rec := serve(f.handler, post); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}
