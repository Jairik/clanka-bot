package voice_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// openListeningSettings creates a SQLite store and settings service with the listening definitions.
func openListeningSettings(t *testing.T) (*settings.Service, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	svc, err := settings.New(context.Background(), st, voice.ListeningDefinitions()...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	return svc, st
}

// TestIsListeningLogic verifies the pure boolean and timestamp calculation.
func TestIsListeningLogic(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(15 * time.Minute).Format(time.RFC3339)
	past := now.Add(-15 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name      string
		listening bool
		until     string
		now       time.Time
		want      bool
	}{
		{name: "default active", listening: true, until: "", now: now, want: true},
		{name: "indefinite pause", listening: false, until: "", now: now, want: false},
		{name: "timed pause in future", listening: true, until: future, now: now, want: false},
		{name: "timed pause in past", listening: true, until: past, now: now, want: true},
		{name: "indefinite pause with future timestamp", listening: false, until: future, now: now, want: false},
		{name: "indefinite pause with past timestamp", listening: false, until: past, now: now, want: false},
		{name: "invalid timestamp does not gate", listening: true, until: "not-a-timestamp", now: now, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := voice.IsListening(tt.listening, tt.until, tt.now)
			if got != tt.want {
				t.Errorf("IsListening(%v, %q) = %v, want %v", tt.listening, tt.until, got, tt.want)
			}
		})
	}
}

// TestListeningSettingsValidationAndOverrides covers definition defaults,
// type/timestamp validation, stored overrides, and reset back to default.
func TestListeningSettingsValidationAndOverrides(t *testing.T) {
	ctx := context.Background()
	svc, st := openListeningSettings(t)

	// Check initial defaults.
	listenVal, ok := svc.Get(voice.ListeningKey)
	if !ok || listenVal.Value != true || listenVal.Source != settings.SourceDefault {
		t.Fatalf("listening default = %+v, want true (default)", listenVal)
	}
	untilVal, ok := svc.Get(voice.ListeningUntilKey)
	if !ok || untilVal.Value != "" || untilVal.Source != settings.SourceDefault {
		t.Fatalf("listening_until default = %+v, want empty string (default)", untilVal)
	}

	// Validation on wakeword.listening: non-boolean must be rejected.
	if _, err := svc.Set(ctx, voice.ListeningKey, "not-a-bool"); err == nil {
		t.Errorf("expected error setting non-bool for wakeword.listening, got nil")
	}

	// Validation on wakeword.listening_until: non-string and non-RFC3339 must be rejected.
	if _, err := svc.Set(ctx, voice.ListeningUntilKey, 12345); err == nil {
		t.Errorf("expected error setting int for wakeword.listening_until, got nil")
	}
	if _, err := svc.Set(ctx, voice.ListeningUntilKey, "invalid-time"); err == nil {
		t.Errorf("expected error setting invalid timestamp for wakeword.listening_until, got nil")
	}

	// Valid RFC3339 timestamp is accepted.
	validUntil := "2026-11-17T13:30:00Z"
	if _, err := svc.Set(ctx, voice.ListeningUntilKey, validUntil); err != nil {
		t.Fatalf("Set valid timestamp: %v", err)
	}
	if v, _ := svc.Get(voice.ListeningUntilKey); v.Value != validUntil || v.Source != settings.SourceOverride {
		t.Errorf("listening_until = %+v, want override %s", v, validUntil)
	}

	// Stored override for wakeword.listening.
	if _, err := svc.Set(ctx, voice.ListeningKey, false); err != nil {
		t.Fatalf("Set wakeword.listening = false: %v", err)
	}
	if v, _ := svc.Get(voice.ListeningKey); v.Value != false || v.Source != settings.SourceOverride {
		t.Errorf("listening = %+v, want override false", v)
	}

	// Resetting both returns them to code default.
	if _, err := svc.Reset(ctx, voice.ListeningKey); err != nil {
		t.Fatalf("Reset listening: %v", err)
	}
	if v, _ := svc.Get(voice.ListeningKey); v.Value != true || v.Source != settings.SourceDefault {
		t.Errorf("after Reset, listening = %+v, want true default", v)
	}
	if _, err := svc.Reset(ctx, voice.ListeningUntilKey); err != nil {
		t.Fatalf("Reset listening_until: %v", err)
	}
	if v, _ := svc.Get(voice.ListeningUntilKey); v.Value != "" || v.Source != settings.SourceDefault {
		t.Errorf("after Reset, listening_until = %+v, want empty default", v)
	}

	_ = st
}

// TestListeningSeamTransitionAndReadPath verifies that while listening_until is in
// the future the seam reports false, and once it is in the past it reports true again,
// with no write on the read path.
func TestListeningSeamTransitionAndReadPath(t *testing.T) {
	ctx := context.Background()
	svc, st := openListeningSettings(t)

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(15 * time.Minute)

	// Set a timed pause: listening=true, listening_until in future.
	if err := svc.SetMany(ctx, map[string]any{
		voice.ListeningKey:      true,
		voice.ListeningUntilKey: future.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	// Record database change count before read checks.
	countRows := func() int {
		var count int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM settings").Scan(&count); err != nil {
			t.Fatalf("query settings count: %v", err)
		}
		return count
	}
	initialRows := countRows()

	// While listening_until is in the future, ListeningState reports false.
	active, until := voice.ListeningState(svc, now)
	if active {
		t.Errorf("ListeningState before expiry = true, want false")
	}
	if until != future.Format(time.RFC3339) {
		t.Errorf("until = %q, want %q", until, future.Format(time.RFC3339))
	}

	// 1 second after future: it has expired, so ListeningState reports true.
	afterExpiry := future.Add(time.Second)
	activeAfter, untilAfter := voice.ListeningState(svc, afterExpiry)
	if !activeAfter {
		t.Errorf("ListeningState after expiry = false, want true")
	}
	if untilAfter != future.Format(time.RFC3339) {
		t.Errorf("until after expiry = %q, want preserved %q", untilAfter, future.Format(time.RFC3339))
	}

	// Verify no write happened on the read path.
	if rows := countRows(); rows != initialRows {
		t.Errorf("row count changed on read path: got %d, want %d", rows, initialRows)
	}

	// Indefinite pause stays false until Resume clears it.
	if err := svc.SetMany(ctx, map[string]any{
		voice.ListeningKey:      false,
		voice.ListeningUntilKey: "",
	}); err != nil {
		t.Fatalf("SetMany indefinite: %v", err)
	}
	activeIndef, _ := voice.ListeningState(svc, now)
	if activeIndef {
		t.Errorf("indefinite pause = active true, want false")
	}
	activeIndefFar, _ := voice.ListeningState(svc, now.Add(48*time.Hour))
	if activeIndefFar {
		t.Errorf("indefinite pause after 48h = active true, want false")
	}

	// Resume clears the indefinite pause.
	if err := svc.SetMany(ctx, map[string]any{
		voice.ListeningKey:      true,
		voice.ListeningUntilKey: "",
	}); err != nil {
		t.Fatalf("SetMany resume: %v", err)
	}
	activeResumed, _ := voice.ListeningState(svc, now)
	if !activeResumed {
		t.Errorf("after resume = active false, want true")
	}
}
