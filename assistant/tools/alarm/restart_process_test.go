package alarm_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// The environment variables name the two halves of the process-restart test.
const (
	restartModeEnv  = "ALARM_RESTART_MODE"
	restartDBEnv    = "ALARM_RESTART_DB"
	restartAlertEnv = "ALARM_RESTART_ALERT"
)

// restartNow is the injected wall time both child processes use. The Alarm is
// 100 ms ahead, so the recovery child Rings promptly without a wall-clock wait.
var restartNow = time.Date(2026, 11, 12, 6, 59, 59, 900_000_000, time.UTC)

// TestAlarmSurvivesProcessRestart runs the real persistence path in child
// processes: one sets a repeating or one-off Alarm and exits, the next opens
// the same file and must Ring it, and a third must stay silent.
func TestAlarmSurvivesProcessRestart(t *testing.T) {
	if mode := os.Getenv(restartModeEnv); mode != "" {
		runRestartHelper(t, mode)
		return
	}

	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	alertPath := filepath.Join(t.TempDir(), "alert.txt")

	// Repeating Alarm: set, recover and fire, then verify the next occurrence.
	runRestartChild(t, "set-repeat", path, alertPath)
	assertRestartRow(t, path, time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC))
	runRestartChild(t, "fire-repeat", path, alertPath)
	if got := readAlert(t, alertPath); got != "Your alarm is going off." {
		t.Fatalf("repeat alert = %q", got)
	}
	assertRestartRow(t, path, time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC))
	assertRestartHistory(t, path, 1, "stopped", "spoken")
	runRestartChild(t, "silent", path, alertPath)

	// One-off Alarm on its own database: after it fires, the row is gone and a
	// restart is silent.
	oncePath := filepath.Join(t.TempDir(), "data", "assistant.db")
	runRestartChild(t, "set-once", oncePath, alertPath)
	runRestartChild(t, "fire-once", oncePath, alertPath)
	if got := readAlert(t, alertPath); got != "Your alarm is going off." {
		t.Fatalf("one-off alert = %q", got)
	}
	st, err := store.Open(context.Background(), oncePath)
	if err != nil {
		t.Fatalf("open after one-off: %v", err)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&rows); err != nil {
		st.Close()
		t.Fatalf("count alarms: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if rows != 0 {
		t.Fatalf("alarm rows after one-off = %d, want 0", rows)
	}
	runRestartChild(t, "silent", oncePath, alertPath)
}

// runRestartChild runs this test binary with only the restart test selected.
func runRestartChild(t *testing.T, mode, dbPath, alertPath string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAlarmSurvivesProcessRestart$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		restartModeEnv+"="+mode,
		restartDBEnv+"="+dbPath,
		restartAlertEnv+"="+alertPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart child %s failed: %v\n%s", mode, err, output)
	}
}

// readAlert reads the line the firing child wrote.
func readAlert(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read alert file: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// assertRestartRow checks the one persisted Alarm's next occurrence.
func assertRestartRow(t *testing.T, path string, want time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	var next int64
	if err := st.DB().QueryRow(`SELECT next_fires_at FROM alarms`).Scan(&next); err != nil {
		t.Fatalf("read next: %v", err)
	}
	if next != want.UnixMilli() {
		t.Fatalf("next_fires_at = %d, want %d", next, want.UnixMilli())
	}
}

// assertRestartHistory checks the newest Alarm history row.
func assertRestartHistory(t *testing.T, path string, want int, state, endedBy string) {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	var count int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM alert_events WHERE kind = 'alarm' AND state = ? AND ended_by = ?`,
		state, endedBy).Scan(&count); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if count != want {
		t.Fatalf("history %s/%s rows = %d, want %d", state, endedBy, count, want)
	}
}

// runRestartHelper is the child half. It must not call t.Fatalf before its own
// assertions; the exit code is the contract with the parent.
func runRestartHelper(t *testing.T, mode string) {
	ctx := context.Background()
	dbPath := os.Getenv(restartDBEnv)
	alertPath := os.Getenv(restartAlertEnv)
	if dbPath == "" || alertPath == "" {
		t.Fatal("restart helper needs ALARM_RESTART_DB and ALARM_RESTART_ALERT")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("helper store.Open: %v", err)
	}
	defer st.Close()

	clock := &fixedClock{now: restartNow}
	alerts := make(chan string, 4)
	say := func(_ context.Context, text string) error {
		alerts <- text
		return nil
	}
	manager := alarm.NewManager(alarm.Options{
		Now:    clock.Now,
		Say:    say,
		Logger: slog.Default(),
	})
	defer manager.Close()
	if err := manager.AttachStore(st, nil); err != nil {
		t.Fatalf("helper AttachStore: %v", err)
	}

	switch mode {
	case "set-repeat":
		if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7, Schedule: "daily"}); err != nil {
			t.Fatalf("helper Set repeat: %v", err)
		}
	case "set-once":
		if _, err := manager.Set(ctx, alarm.SetIn{Hour: 7}); err != nil {
			t.Fatalf("helper Set once: %v", err)
		}
	case "fire-repeat", "fire-once":
		select {
		case text := <-alerts:
			if err := os.WriteFile(alertPath, []byte(text), 0o600); err != nil {
				t.Fatalf("helper write alert: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("helper did not hear the recovered Alarm")
		}
		// The Alarm is spoken before its outcome is durable. Waiting for the
		// terminal history row (and, for a one-off, the pending-row delete)
		// keeps this exit from racing those writes: exiting on the spoken line
		// alone let the deferred Close cancel the Ring and record
		// interrupted/shutdown, or leave the finished one-off row behind, on a
		// loaded machine.
		waitFireDone(t, manager, st, mode == "fire-once")
	case "silent":
		select {
		case text := <-alerts:
			t.Fatalf("helper repeated an alert: %q", text)
		case <-time.After(500 * time.Millisecond):
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

// waitFireDone blocks until the firing Alarm's durable aftermath is complete:
// its terminal history row, plus the pending-row delete for a one-off Alarm.
// The Ring is spoken before either write, so this is the sync point between
// speech and a clean shutdown.
func waitFireDone(t *testing.T, manager *alarm.Manager, st *store.Store, oneOff bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		settled, err := fireSettled(manager, st, oneOff)
		if err != nil {
			t.Fatalf("helper fire state: %v", err)
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("helper fire never became durable")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fireSettled reports whether the Ring outcome (and, for a one-off Alarm, the
// delete of its finished row) has reached the database.
func fireSettled(manager *alarm.Manager, st *store.Store, oneOff bool) (bool, error) {
	events, err := manager.History(1)
	if err != nil {
		return false, err
	}
	if len(events) != 1 || events[0].Kind != "alarm" || events[0].State.Open() {
		return false, nil
	}
	if !oneOff {
		return true, nil
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM alarms`).Scan(&rows); err != nil {
		return false, err
	}
	return rows == 0, nil
}
