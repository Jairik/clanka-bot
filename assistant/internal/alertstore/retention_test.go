package alertstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// TestRetentionTrimsOldEventsAndCapsRowCount asserts that a fixture of 1200 rows
// spanning 200 days trims to exactly the newest 1000 rows within 90 days. It also
// verifies that a kind whose newest row is older than 90 days is preserved.
func TestRetentionTrimsOldEventsAndCapsRowCount(t *testing.T) {
	ctx := context.Background()
	history, st := openHistory(t)
	now := time.Now().Truncate(time.Second)

	// Insert 1200 rows across 200 days.
	// 100 rows are between 91 and 190 days old (outside the 90-day retention window).
	// 1100 rows are within the last 90 days (days 0 to 89).
	// Both "timer" and "alarm" have rows within the 90-day window, so their newest
	// rows are well within the newest 1000 and do not keep any of the old rows.
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	// 100 rows older than 90 days.
	for i := 0; i < 100; i++ {
		kind := "timer"
		if i%2 == 1 {
			kind = "alarm"
		}
		firedAt := now.Add(-time.Duration(91+i) * 24 * time.Hour)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO alert_events (kind, source_id, label, duration_ms, fires_at, fired_at, state)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			kind, "src", "old", 60000, firedAt.UnixMilli(), firedAt.UnixMilli(), string(alertstore.StateStopped))
		if err != nil {
			t.Fatalf("insert old event %d: %v", i, err)
		}
	}

	// 1100 rows within the last 90 days.
	for i := 0; i < 1100; i++ {
		kind := "timer"
		if i%2 == 1 {
			kind = "alarm"
		}
		// Distribute across days 0 to 89.
		firedAt := now.Add(-time.Duration(i%90)*24*time.Hour - time.Duration(i)*time.Second)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO alert_events (kind, source_id, label, duration_ms, fires_at, fired_at, state)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			kind, "src", "recent", 60000, firedAt.UnixMilli(), firedAt.UnixMilli(), string(alertstore.StateStopped))
		if err != nil {
			t.Fatalf("insert recent event %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	// Verify we inserted 1200 rows.
	var initialCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_events`).Scan(&initialCount); err != nil {
		t.Fatalf("initial count: %v", err)
	}
	if initialCount != 1200 {
		t.Fatalf("initial count = %d, want 1200", initialCount)
	}

	// Prune with default 90 days and 1000 rows.
	res, err := history.Prune(ctx, alertstore.PruneOptions{
		MaxDays: 90,
		MaxRows: 1000,
		Now:     now,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Exactly 200 rows deleted (100 older than 90 days, 100 over the 1000 cap).
	if res.Deleted != 200 {
		t.Errorf("Deleted = %d, want 200", res.Deleted)
	}
	if res.Remaining != 1000 {
		t.Errorf("Remaining = %d, want 1000", res.Remaining)
	}

	// Assert every remaining row is within the 90-day window.
	cutoff := now.Add(-90 * 24 * time.Hour).UnixMilli()
	var outsideCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_events WHERE fired_at < ?`, cutoff).Scan(&outsideCount); err != nil {
		t.Fatalf("count outside window: %v", err)
	}
	if outsideCount != 0 {
		t.Errorf("rows outside 90 days = %d, want 0", outsideCount)
	}

	// Separate assertion: insert a kind whose newest row is older than 90 days.
	// It must be preserved even though it is outside the 90-day window.
	oldFiredAt := now.Add(-120 * 24 * time.Hour)
	_, err = st.DB().ExecContext(ctx, `
		INSERT INTO alert_events (kind, source_id, label, duration_ms, fires_at, fired_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"reminder", "rem1", "old reminder", 0, oldFiredAt.UnixMilli(), oldFiredAt.UnixMilli(), string(alertstore.StateStopped))
	if err != nil {
		t.Fatalf("insert old reminder: %v", err)
	}

	// Run Prune again.
	res2, err := history.Prune(ctx, alertstore.PruneOptions{
		MaxDays: 90,
		MaxRows: 1000,
		Now:     now,
	})
	if err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	if res2.Deleted != 0 {
		t.Errorf("second Prune Deleted = %d, want 0", res2.Deleted)
	}
	if res2.Remaining != 1001 {
		t.Errorf("second Prune Remaining = %d, want 1001 (1000 recent + 1 kept reminder)", res2.Remaining)
	}

	// Verify the reminder row is still present.
	var reminderCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_events WHERE kind = 'reminder'`).Scan(&reminderCount); err != nil {
		t.Fatalf("count reminder: %v", err)
	}
	if reminderCount != 1 {
		t.Errorf("reminder count = %d, want 1 kept", reminderCount)
	}
}

// TestRetentionSettingsValidateAndRegister verifies that history_days and history_rows
// validate input properly and register with the settings service.
func TestRetentionSettingsValidateAndRegister(t *testing.T) {
	_, st := openHistory(t)
	defs := alertstore.Definitions()
	if len(defs) != 2 {
		t.Fatalf("defs count = %d, want 2", len(defs))
	}

	svc, err := settings.New(context.Background(), st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}

	daysVal, ok := svc.Get(alertstore.HistoryDaysKey)
	if !ok || daysVal.Value != 90 {
		t.Errorf("alerts.history_days = %+v, want 90", daysVal)
	}

	rowsVal, ok := svc.Get(alertstore.HistoryRowsKey)
	if !ok || rowsVal.Value != 1000 {
		t.Errorf("alerts.history_rows = %+v, want 1000", rowsVal)
	}

	// Test validation: <= 0 must fail.
	for _, tc := range []struct {
		key   string
		valid int
	}{
		{alertstore.HistoryDaysKey, 30},
		{alertstore.HistoryRowsKey, 500},
	} {
		if _, err := svc.Set(context.Background(), tc.key, tc.valid); err != nil {
			t.Errorf("Set %s to %d failed: %v", tc.key, tc.valid, err)
		}
		if _, err := svc.Set(context.Background(), tc.key, 0); err == nil {
			t.Errorf("Set %s to 0 should fail validator", tc.key)
		}
		if _, err := svc.Set(context.Background(), tc.key, -5); err == nil {
			t.Errorf("Set %s to -5 should fail validator", tc.key)
		}
	}
}
