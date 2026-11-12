package reminder_test

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
)

// openReminderStore opens the real database with the real migrations. Tests
// close it explicitly when they simulate a process restart.
func openReminderStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// storedManager attaches the store exactly as the module does. The queue
// speaker handles ordinary Announcements; the recorder handles the urgent
// Reminder item.
func storedManager(t *testing.T, st *store.Store, now func() time.Time, rec *traceRecorder, announcements *announce.Queue) *reminder.Manager {
	t.Helper()
	if announcements == nil {
		// The Manager needs the queue to play an urgent item; the recorder is
		// also the queue speaker, so a plain announcement is observable too.
		announcements = announce.New(rec, testLogger())
		t.Cleanup(announcements.Close)
	}
	m := reminder.NewManager(reminder.Options{Now: now, Speaker: rec, Queue: announcements, Logger: testLogger()})
	if err := m.AttachStore(st); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// countReminderRows counts persisted Reminders through the public store handle.
func countReminderRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM reminders`).Scan(&count); err != nil {
		t.Fatalf("count reminders: %v", err)
	}
	return count
}

// seedReminder inserts a row as a previous process would have left it.
func seedReminder(t *testing.T, st *store.Store, message string, kind reminder.Kind, duration time.Duration, firesAt time.Time) {
	t.Helper()
	if _, err := st.DB().Exec(
		`INSERT INTO reminders (message, kind, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		message, string(kind), duration.Milliseconds(), firesAt.UnixMilli(), firesAt.Add(-duration).UnixMilli()); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
}

// TestRemindersAndEveryOtherTableShareOneDatabase pins the merged ledger:
// version 7 is Alarms (#53) and version 8 is Reminders.
func TestRemindersAndEveryOtherTableShareOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st := openReminderStore(t, path)
	defer st.Close()

	rows, err := st.DB().Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	defer rows.Close()
	applied := map[int]string{}
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatalf("scan migration: %v", err)
		}
		applied[version] = name
	}
	want := map[int]string{
		1: "settings", 2: "secrets", 3: "timers", 4: "sounds",
		5: "alert_events", 6: "voices", 7: "alarms", 8: "reminders",
	}
	if len(applied) != len(want) {
		t.Fatalf("fresh migrations = %v, want %v", applied, want)
	}
	for version, name := range want {
		if applied[version] != name {
			t.Fatalf("fresh migration %d = %q, want %q", version, applied[version], name)
		}
	}
}

// TestReminderSurvivesRestart is the persistence criterion: a Reminder set
// before a restart still speaks after it, once.
func TestReminderSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")

	firstStore := openReminderStore(t, path)
	firstRec := newTraceRecorder()
	first := storedManager(t, firstStore, nil, firstRec, nil)
	started, err := first.StartDuration("check the oven", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if started.ID == "" || started.Message != "check the oven" {
		t.Fatalf("StartDuration returned %+v", started)
	}
	if got := countReminderRows(t, firstStore); got != 1 {
		t.Fatalf("persisted rows = %d, want 1", got)
	}
	first.Close()
	if err := firstStore.Close(); err != nil {
		t.Fatalf("first store close: %v", err)
	}

	secondStore := openReminderStore(t, path)
	defer secondStore.Close()
	secondRec := newTraceRecorder()
	second := storedManager(t, secondStore, nil, secondRec, nil)
	if got := len(second.List()); got != 1 {
		t.Fatalf("recovered reminders = %d, want 1", got)
	}
	if got := secondRec.wait(t, 3*time.Second); got != "check the oven" {
		t.Fatalf("spoken = %q, want the recovered reminder", got)
	}
	if got := countReminderRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows after firing = %d, want 0", got)
	}
	secondRec.expectSilence(t, 150*time.Millisecond)
}

// TestNoDuplicateSpeechAfterRestart proves at-most-once: once the fire
// transaction removed the row, reopening the database cannot say it again.
func TestNoDuplicateSpeechAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")

	firstStore := openReminderStore(t, path)
	firstRec := newTraceRecorder()
	first := storedManager(t, firstStore, nil, firstRec, nil)
	if _, err := first.StartDuration("check the oven", 20*time.Millisecond); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	firstRec.wait(t, 2*time.Second)
	first.Close()
	firstStore.Close()

	secondStore := openReminderStore(t, path)
	defer secondStore.Close()
	secondRec := newTraceRecorder()
	second := storedManager(t, secondStore, nil, secondRec, nil)
	if got := len(second.List()); got != 0 {
		t.Fatalf("recovered reminders after fire = %d, want 0", got)
	}
	secondRec.expectSilence(t, 250*time.Millisecond)
	if got := countReminderRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows after duplicate check = %d, want 0", got)
	}
}

// TestOverdueGraceBoundary checks the exact downtime rule with an injected
// clock: exactly two minutes overdue speaks now, one millisecond more is in
// the summary.
func TestOverdueGraceBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.UnixMilli(time.Now().UnixMilli())

	firstStore := openReminderStore(t, path)
	seedReminder(t, firstStore, "check the oven", reminder.KindDuration, 10*time.Minute, now.Add(-2*time.Minute))
	seedReminder(t, firstStore, "call mum", reminder.KindDuration, 10*time.Minute, now.Add(-2*time.Minute-time.Millisecond))
	firstStore.Close()

	secondStore := openReminderStore(t, path)
	defer secondStore.Close()
	rec := newTraceRecorder()
	announcementRec := newTraceRecorder()
	queue := announce.New(announcementRec, slog.Default())
	defer queue.Close()

	storedManager(t, secondStore, func() time.Time { return now }, rec, queue)

	if got := rec.wait(t, 2*time.Second); got != "check the oven" {
		t.Fatalf("grace reminder = %q, want the oven message", got)
	}
	if got := announcementRec.wait(t, 2*time.Second); got != "You missed a reminder: call mum." {
		t.Fatalf("summary = %q, want the older reminder", got)
	}
	rec.expectSilence(t, 100*time.Millisecond)
	if got := countReminderRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows = %d, want 0", got)
	}
}

// TestOldOverdueRemindersBecomeOneSummaryAnnouncement covers the downtime
// summary: older Reminders are named once, not fired one by one.
func TestOldOverdueRemindersBecomeOneSummaryAnnouncement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.Now()

	firstStore := openReminderStore(t, path)
	seedReminder(t, firstStore, "check the oven", reminder.KindDuration, 10*time.Minute, now.Add(-40*time.Minute))
	seedReminder(t, firstStore, "call mum", reminder.KindDuration, 5*time.Minute, now.Add(-20*time.Minute))
	seedReminder(t, firstStore, "check the oven", reminder.KindDuration, 30*time.Minute, now.Add(-3*time.Minute))
	firstStore.Close()

	secondStore := openReminderStore(t, path)
	defer secondStore.Close()
	rec := newTraceRecorder()
	announcementRec := newTraceRecorder()
	queue := announce.New(announcementRec, slog.Default())
	defer queue.Close()

	storedManager(t, secondStore, func() time.Time { return now }, rec, queue)

	if got := announcementRec.wait(t, 2*time.Second); got != "You missed 3 reminders: check the oven, call mum." {
		t.Fatalf("summary = %q", got)
	}
	// One summary only: the queue is drained and no reminder speaks alone.
	select {
	case extra := <-announcementRec.said:
		t.Fatalf("second summary announcement: %q", extra)
	case text := <-rec.said:
		t.Fatalf("an old reminder spoke alone: %q", text)
	case <-time.After(150 * time.Millisecond):
	}
	if got := countReminderRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows = %d, want 0", got)
	}
}

// TestCancelIsPersistent proves a cancelled Reminder does not come back.
func TestCancelIsPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")

	firstStore := openReminderStore(t, path)
	first := storedManager(t, firstStore, nil, newTraceRecorder(), nil)
	if _, err := first.StartDuration("check the oven", time.Hour); err != nil {
		t.Fatalf("StartDuration: %v", err)
	}
	if cancelled, err := first.CancelQuery("oven"); err != nil || cancelled != 1 {
		t.Fatalf("CancelQuery = %d, %v; want 1, nil", cancelled, err)
	}
	first.Close()
	firstStore.Close()

	secondStore := openReminderStore(t, path)
	defer secondStore.Close()
	secondRec := newTraceRecorder()
	second := storedManager(t, secondStore, nil, secondRec, nil)
	if got := len(second.List()); got != 0 {
		t.Fatalf("recovered reminders after cancel = %d, want 0", got)
	}
	if got := countReminderRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows after cancel = %d, want 0", got)
	}
	secondRec.expectSilence(t, 100*time.Millisecond)
}

// TestAttachStoreReadsRowsWrittenByAnotherHandle keeps the persistence seam
// honest: the Manager reads the table, not an in-memory copy.
func TestAttachStoreReadsRowsWrittenByAnotherHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.Now()
	st := openReminderStore(t, path)
	defer st.Close()
	seedReminder(t, st, "water the plants", reminder.KindDuration, time.Hour, now.Add(time.Hour))

	m := storedManager(t, st, func() time.Time { return now }, newTraceRecorder(), nil)
	list := m.List()
	if len(list) != 1 || list[0].Message != "water the plants" {
		t.Fatalf("List = %+v, want the seeded reminder", list)
	}
	if got, want := list[0].RemainingSeconds, 3600; got != want {
		t.Fatalf("RemainingSeconds = %d, want %d", got, want)
	}
}

// TestOpenFiredEventBecomesMissedOnRestart proves a crash after the fire
// transaction is recorded as missed and never spoken again.
func TestOpenFiredEventBecomesMissedOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.Now()
	st := openReminderStore(t, path)
	history := alertstore.New(st.DB())
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := history.InsertTx(context.Background(), tx, alertstore.Event{
		Kind: "reminder", SourceID: "99", Label: "check the oven",
		FiresAt: now.Add(-time.Minute), FiredAt: now.Add(-time.Minute), State: alertstore.StateFired,
	}); err != nil {
		t.Fatalf("insert open event: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rec := newTraceRecorder()
	m := storedManager(t, st, func() time.Time { return now }, rec, nil)
	rec.expectSilence(t, 150*time.Millisecond)
	events, err := m.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(events) != 1 || events[0].State != alertstore.StateMissed || events[0].EndedBy != "restart" {
		t.Fatalf("history = %+v, want one missed/restart row", events)
	}
}

// TestDowntimeSummaryDropDoesNotLogTheMessage keeps a headless recovery from
// writing personal Reminder text to the log.
func TestDowntimeSummaryDropDoesNotLogTheMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.Now()
	firstStore := openReminderStore(t, path)
	seedReminder(t, firstStore, "buy a birthday present for Sam", reminder.KindDuration, 10*time.Minute, now.Add(-40*time.Minute))
	firstStore.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	secondStore := openReminderStore(t, path)
	defer secondStore.Close()
	m := reminder.NewManager(reminder.Options{Now: func() time.Time { return now }, Logger: logger})
	if err := m.AttachStore(secondStore); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	t.Cleanup(m.Close)

	output := logs.String()
	if strings.Contains(output, "birthday") || strings.Contains(output, "Sam") {
		t.Fatalf("recovery logs leaked the reminder message:\n%s", output)
	}
	if !strings.Contains(output, "reminders=1") && !strings.Contains(output, "reminders 1") {
		t.Fatalf("recovery logs lost the dropped-summary count:\n%s", output)
	}
}
