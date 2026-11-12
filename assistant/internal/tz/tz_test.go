package tz_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/settingsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
)

// newSettings opens a real temp database and builds the settings service with
// the timezone definition, exactly as main does.
func newSettings(t *testing.T, configValue string) (*settings.Service, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc, err := settings.New(context.Background(), st, tz.Definition(configValue))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc, st
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// TestDefinitionValidatesIANANames keeps an unusable timezone out of the store
// and out of config.yaml.
func TestDefinitionValidatesIANANames(t *testing.T) {
	valid := []string{"", "UTC", "Europe/Berlin", "America/New_York", "Asia/Tokyo"}
	for _, name := range valid {
		if err := tz.Validate(name); err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
	}
	invalid := []string{"Local", "/etc/localtime", "../zoneinfo", "Mars/Olympus"}
	for _, name := range invalid {
		if err := tz.Validate(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

// TestLocationPrefersSettingThenTZThenUTC is the fallback chain.
func TestLocationPrefersSettingThenTZThenUTC(t *testing.T) {
	svc, _ := newSettings(t, "")
	zone := tz.New("America/New_York", testLogger())
	zone.Bind(svc)
	if got := zone.Location().String(); got != "America/New_York" {
		t.Fatalf("TZ fallback = %q, want America/New_York", got)
	}

	// A stored override applies live, without recreating the Service.
	if _, err := svc.Set(context.Background(), tz.Key, "Asia/Tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := zone.Location().String(); got != "Asia/Tokyo" {
		t.Fatalf("override = %q, want Asia/Tokyo", got)
	}

	// Reset returns to the TZ layer.
	if _, err := svc.Reset(context.Background(), tz.Key); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := zone.Location().String(); got != "America/New_York" {
		t.Fatalf("after reset = %q, want America/New_York", got)
	}

	// With no setting and no TZ, UTC is the documented safe default.
	unset := tz.New("", testLogger())
	unset.Bind(svc)
	if !unset.IsUTC() {
		t.Fatalf("unset zone = %q, want UTC", unset.Location())
	}

	invalid := tz.New("Not/AZone", testLogger())
	if !invalid.IsUTC() {
		t.Fatalf("invalid TZ = %q, want UTC", invalid.Location())
	}
}

// TestConfigLayerSuppliesTheTimezone proves config.yaml is the middle layer.
func TestConfigLayerSuppliesTheTimezone(t *testing.T) {
	svc, _ := newSettings(t, "Europe/Berlin")
	zone := tz.New("America/New_York", testLogger())
	zone.Bind(svc)
	if got := zone.Location().String(); got != "Europe/Berlin" {
		t.Fatalf("config layer = %q, want Europe/Berlin", got)
	}
	if _, err := svc.Set(context.Background(), tz.Key, "Asia/Tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := zone.Location().String(); got != "Asia/Tokyo" {
		t.Fatalf("override = %q, want Asia/Tokyo", got)
	}
}

// TestDashboardSettingWriteChangesTheZoneLive proves the Dashboard and the
// timezone seam use the same settings service: a PUT through the generic
// settings handler changes the location with no restart.
func TestDashboardSettingWriteChangesTheZoneLive(t *testing.T) {
	svc, _ := newSettings(t, "")
	zone := tz.New("America/New_York", testLogger())
	zone.Bind(svc)
	handler := settingsapi.New(svc, nil, testLogger())

	body, err := json.Marshal(map[string]any{"value": "Europe/Berlin"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/settings/timezone", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT /settings/timezone = %d: %s", response.Code, response.Body.String())
	}
	if got := zone.Location().String(); got != "Europe/Berlin" {
		t.Fatalf("location after Dashboard write = %q, want Europe/Berlin", got)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/settings/timezone", nil)
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("DELETE /settings/timezone = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if got := zone.Location().String(); got != "America/New_York" {
		t.Fatalf("location after reset = %q, want America/New_York", got)
	}
}

// TestInvalidDashboardWriteIsRejected keeps the store free of bad zone names.
func TestInvalidDashboardWriteIsRejected(t *testing.T) {
	svc, _ := newSettings(t, "")
	handler := settingsapi.New(svc, nil, testLogger())
	body := []byte(`{"value":"Mars/Olympus"}`)
	request := httptest.NewRequest(http.MethodPut, "/settings/timezone", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid timezone = %d, want 400: %s", response.Code, response.Body.String())
	}
	if value, _ := svc.Get(tz.Key); value.Source != settings.SourceDefault {
		t.Fatalf("invalid write changed the source to %q", value.Source)
	}
}

// TestLoadedZoneUsesTheEmbeddedDatabase pins the IANA source: a zone that the
// host lacks must still resolve from time/tzdata.
func TestLoadedZoneUsesTheEmbeddedDatabase(t *testing.T) {
	location, err := tz.Load("Pacific/Auckland")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if location == nil {
		t.Fatal("Load returned nil for a valid zone")
	}
	if _, err := tz.Load(""); err != nil {
		t.Fatalf("empty name: %v", err)
	}
	if location, err := tz.Load(""); location != nil || err != nil {
		t.Fatalf("Load(\"\") = %v, %v; want nil, nil", location, err)
	}
}

// TestNameReportsTheEffectiveZone covers the startup log and warning input.
func TestNameReportsTheEffectiveZone(t *testing.T) {
	svc, _ := newSettings(t, "Europe/Berlin")
	zone := tz.New("", testLogger())
	zone.Bind(svc)
	if got := zone.Name(); got != "Europe/Berlin" {
		t.Fatalf("Name = %q, want Europe/Berlin", got)
	}
	unbound := tz.New("", testLogger())
	if got := unbound.Name(); got != "UTC" {
		t.Fatalf("unbound Name = %q, want UTC", got)
	}
	if !unbound.IsUTC() {
		t.Fatal("unbound IsUTC = false")
	}
}
