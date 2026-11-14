package reminder_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// The environment variable names the two halves of the process-restart test.
const (
	restartModeEnv    = "REMINDER_RESTART_MODE"
	restartDBEnv      = "REMINDER_RESTART_DB"
	restartSpeechEnv  = "REMINDER_RESTART_SPEECH"
	restartMessageEnv = "REMINDER_RESTART_MESSAGE"
)

// TestReminderSurvivesProcessRestart runs the real persistence path in two
// child processes: one sets a Reminder and exits, the next opens the same file
// and must speak its message. A test binary is the child, so no production
// binary or TTS service is needed.
func TestReminderSurvivesProcessRestart(t *testing.T) {
	if mode := os.Getenv(restartModeEnv); mode != "" {
		runRestartHelper(t, mode)
		return
	}

	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	speechPath := filepath.Join(t.TempDir(), "speech.txt")
	const message = "check the oven"

	runRestartChild(t, "set", path, speechPath, message)

	// A parent-side check reads the row the first process committed, then moves
	// it just inside the downtime grace. The set child must not race a real
	// timer to the row, and the firing child must find it due across the
	// restart rather than wait an hour for it.
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store after set: %v", err)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM reminders`).Scan(&rows); err != nil {
		st.Close()
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		st.Close()
		t.Fatalf("persisted rows after first process = %d, want 1", rows)
	}
	makeDue(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	runRestartChild(t, "fire", path, speechPath, message)

	raw, err := os.ReadFile(speechPath)
	if err != nil {
		t.Fatalf("read speech file: %v", err)
	}
	if got, want := strings.TrimSpace(string(raw)), message; got != want {
		t.Fatalf("speech = %q, want %q", got, want)
	}

	// The firing process deleted the row before speaking, so a third start
	// cannot repeat it. The history row is the durable record.
	st, err = store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store after fire: %v", err)
	}
	defer st.Close()
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM reminders`).Scan(&rows); err != nil {
		t.Fatalf("count rows after fire: %v", err)
	}
	if rows != 0 {
		t.Fatalf("persisted rows after fire = %d, want 0", rows)
	}
	var state, endedBy string
	if err := st.DB().QueryRow(
		`SELECT state, ended_by FROM alert_events WHERE kind = 'reminder' ORDER BY id DESC LIMIT 1`,
	).Scan(&state, &endedBy); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if state != string(alertstore.StateStopped) || endedBy != "spoken" {
		t.Fatalf("history = %s/%s, want stopped/spoken", state, endedBy)
	}

	runRestartChild(t, "silent", path, speechPath, message)
}

// runRestartChild runs this test binary with only the restart test selected.
func runRestartChild(t *testing.T, mode, dbPath, speechPath, message string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReminderSurvivesProcessRestart$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		restartModeEnv+"="+mode,
		restartDBEnv+"="+dbPath,
		restartSpeechEnv+"="+speechPath,
		restartMessageEnv+"="+message,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart child %s failed: %v\n%s", mode, err, output)
	}
}

// speechFunc adapts a function to the Announcement and Manager speech seams.
type speechFunc func(context.Context, string) error

func (f speechFunc) SayText(ctx context.Context, text string) error { return f(ctx, text) }

// runRestartHelper is the child half. It must not call t.Fatalf before the
// child's own assertions; an exit code is the contract with the parent.
func runRestartHelper(t *testing.T, mode string) {
	ctx := context.Background()
	dbPath := os.Getenv(restartDBEnv)
	speechPath := os.Getenv(restartSpeechEnv)
	message := os.Getenv(restartMessageEnv)
	if dbPath == "" || speechPath == "" || message == "" {
		t.Fatal("restart helper needs the DB, speech path and message environment")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("helper store.Open: %v", err)
	}
	defer st.Close()

	speech := make(chan string, 1)
	say := func(_ context.Context, text string) error {
		speech <- text
		return nil
	}
	speaker := speechFunc(say)
	queue := announce.New(speaker, slog.Default())
	defer queue.Close()
	m := reminder.NewManager(reminder.Options{Speaker: speaker, Queue: queue, Logger: slog.Default()})
	if err := m.AttachStore(st); err != nil {
		t.Fatalf("helper AttachStore: %v", err)
	}
	defer m.Close()

	switch mode {
	case "set":
		// An hour out: the child exits long before it is due, and the parent
		// moves it due across the restart. A 400 ms duration used to race this
		// process's own teardown and fire before the row could be observed.
		if _, err := m.StartDuration(message, time.Hour); err != nil {
			t.Fatalf("helper StartDuration: %v", err)
		}
	case "fire":
		select {
		case text := <-speech:
			if err := os.WriteFile(speechPath, []byte(text), 0o600); err != nil {
				t.Fatalf("helper write speech: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("helper did not hear the recovered Reminder")
		}
		// The message is spoken before its outcome is durable. Waiting for the
		// terminal history row keeps this exit from racing that write, which a
		// busy machine otherwise resolves as interrupted/shutdown.
		waitReminderOutcome(t, m)
	case "silent":
		if list := m.List(); len(list) != 0 {
			t.Fatalf("helper recovered %d reminders, want none", len(list))
		}
		select {
		case text := <-speech:
			t.Fatalf("helper repeated a reminder: %q", text)
		case <-time.After(time.Second):
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

// waitReminderOutcome blocks until the firing Reminder's outcome is durable.
// The message is spoken before the state is written, so this is the sync point
// between speech and a clean shutdown.
func waitReminderOutcome(t *testing.T, m *reminder.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := m.History(1)
		if err != nil {
			t.Fatalf("helper history: %v", err)
		}
		if len(events) == 1 && events[0].Kind == "reminder" && !events[0].State.Open() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper reminder outcome never became durable: %+v", events)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
