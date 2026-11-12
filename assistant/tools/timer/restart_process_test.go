package timer_test

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
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// The environment variable names the two halves of the process-restart test.
const (
	restartModeEnv  = "TIMER_RESTART_MODE"
	restartDBEnv    = "TIMER_RESTART_DB"
	restartAlertEnv = "TIMER_RESTART_ALERT"
)

// TestTimerSurvivesProcessRestart runs the real persistence path in two child
// processes: one sets a Timer and exits, the next opens the same file and must
// speak its alert. A test binary is the child so no production binary or TTS
// service is needed.
func TestTimerSurvivesProcessRestart(t *testing.T) {
	if mode := os.Getenv(restartModeEnv); mode != "" {
		runRestartHelper(t, mode)
		return
	}

	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	alertPath := filepath.Join(t.TempDir(), "alert.txt")

	runRestartChild(t, "set", path, alertPath)

	// A parent-side check reads the row the first process committed.
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store after set: %v", err)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM timers`).Scan(&rows); err != nil {
		st.Close()
		t.Fatalf("count rows: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if rows != 1 {
		t.Fatalf("persisted rows after first process = %d, want 1", rows)
	}

	runRestartChild(t, "fire", path, alertPath)

	raw, err := os.ReadFile(alertPath)
	if err != nil {
		t.Fatalf("read alert file: %v", err)
	}
	if got, want := strings.TrimSpace(string(raw)), "Tea timer is done."; got != want {
		t.Fatalf("alert = %q, want %q", got, want)
	}

	// The firing process deleted the row before speaking, so a third start
	// cannot repeat it.
	st, err = store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store after fire: %v", err)
	}
	defer st.Close()
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM timers`).Scan(&rows); err != nil {
		t.Fatalf("count rows after fire: %v", err)
	}
	if rows != 0 {
		t.Fatalf("persisted rows after fire = %d, want 0", rows)
	}
}

// runRestartChild runs this test binary with only the restart test selected.
func runRestartChild(t *testing.T, mode, dbPath, alertPath string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTimerSurvivesProcessRestart$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		restartModeEnv+"="+mode,
		restartDBEnv+"="+dbPath,
		restartAlertEnv+"="+alertPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart child %s failed: %v\n%s", mode, err, output)
	}
}

// runRestartHelper is the child half. It must not call t.Fatalf before the
// child's own assertions; an exit code is the contract with the parent.
func runRestartHelper(t *testing.T, mode string) {
	ctx := context.Background()
	dbPath := os.Getenv(restartDBEnv)
	alertPath := os.Getenv(restartAlertEnv)
	if dbPath == "" || alertPath == "" {
		t.Fatal("restart helper needs TIMER_RESTART_DB and TIMER_RESTART_ALERT")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("helper store.Open: %v", err)
	}
	defer st.Close()

	alerts := make(chan string, 1)
	say := func(_ context.Context, text string) error {
		alerts <- text
		return nil
	}
	m := timer.NewManagerText(nil, say, slog.Default(), timer.AlertText)
	if err := m.AttachStore(st, nil); err != nil {
		t.Fatalf("helper AttachStore: %v", err)
	}
	defer m.Close()

	switch mode {
	case "set":
		if _, err := m.Start("tea", 400*time.Millisecond); err != nil {
			t.Fatalf("helper Start: %v", err)
		}
	case "fire":
		// AttachStore recovers the row; it is overdue by the time this process
		// starts, or fires shortly after. Either way the alert must arrive.
		select {
		case text := <-alerts:
			if err := os.WriteFile(alertPath, []byte(text), 0o600); err != nil {
				t.Fatalf("helper write alert: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("helper did not hear the recovered Timer")
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}
