package timer_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// openTimerStore opens the real database with the real migrations. Tests close
// it explicitly when they simulate a process restart.
func openTimerStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// storedManager attaches the store exactly as the module does.
func storedManager(t *testing.T, st *store.Store, now func() time.Time, rec *alertRecorder, q *announce.Queue) *timer.Manager {
	t.Helper()
	var say func(context.Context, string) error
	if rec != nil {
		say = rec.SayText
	}
	m := timer.NewManagerText(now, say, slog.Default(), timer.AlertText)
	if err := m.AttachStore(st, q); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// countTimerRows counts persisted Timers through the public store handle.
func countTimerRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM timers`).Scan(&count); err != nil {
		t.Fatalf("count timers: %v", err)
	}
	return count
}

// insertSavedTimer seeds a row as a previous process would have left it.
func insertSavedTimer(t *testing.T, st *store.Store, label string, duration time.Duration, firesAt time.Time) {
	t.Helper()
	if _, err := st.DB().Exec(
		`INSERT INTO timers (label, duration_ms, fires_at, created_at) VALUES (?, ?, ?, ?)`,
		label, duration.Milliseconds(), firesAt.UnixMilli(), firesAt.Add(-duration).UnixMilli()); err != nil {
		t.Fatalf("seed timer: %v", err)
	}
}

// waitAlert receives one alert line or fails the test.
func waitAlert(t *testing.T, rec *alertRecorder) string {
	t.Helper()
	select {
	case text := <-rec.texts:
		return text
	case <-time.After(3 * time.Second):
		t.Fatal("timer did not fire an alert")
		return ""
	}
}

// expectNoAlert proves a cancelled or already-fired Timer stays silent.
func expectNoAlert(t *testing.T, rec *alertRecorder, wait time.Duration) {
	t.Helper()
	select {
	case text := <-rec.texts:
		t.Fatalf("unexpected timer alert: %q", text)
	case <-time.After(wait):
	}
}

// announcementRecorder is the announce.Speaker seam for downtime summaries.
type announcementRecorder struct {
	texts chan string
}

func newAnnouncementRecorder() *announcementRecorder {
	return &announcementRecorder{texts: make(chan string, 4)}
}

func (r *announcementRecorder) SayText(_ context.Context, text string) error {
	r.texts <- text
	return nil
}

func waitAnnouncement(t *testing.T, rec *announcementRecorder) string {
	t.Helper()
	select {
	case text := <-rec.texts:
		return text
	case <-time.After(3 * time.Second):
		t.Fatal("downtime summary did not play")
		return ""
	}
}

// TestSecretsAndTimersPreserveEachOtherAcrossReopen pins the merged migration
// ledger: #46's secrets table (version 2), #51's timers table (version 3) and
// #55's sound library index (version 4) share one database and must both
// survive a fresh apply and a reopen.
func TestSecretsAndTimersPreserveEachOtherAcrossReopen(t *testing.T) {
	const name = "TIMER_MERGE_TEST_SECRET"
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	ctx := context.Background()

	firstStore := openTimerStore(t, path)
	secretSvc, err := secrets.New(ctx, firstStore.DB(), []string{name})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	if _, err := secretSvc.Set(ctx, name, "secret-value-1234"); err != nil {
		t.Fatalf("secret Set: %v", err)
	}
	first := storedManager(t, firstStore, nil, newRecorder(), nil)
	if _, err := first.Start("pasta", time.Hour); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A fresh database applies every migration in order.
	migrations := func(st *store.Store) map[int]string {
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
		if err := rows.Err(); err != nil {
			t.Fatalf("read migrations: %v", err)
		}
		return applied
	}
	wantVersions := map[int]string{1: "settings", 2: "secrets", 3: "timers", 4: "sounds", 5: "alert_events"}
	applied := migrations(firstStore)
	if len(applied) != len(wantVersions) {
		t.Fatalf("fresh migrations = %v, want %v", applied, wantVersions)
	}
	for version, name := range wantVersions {
		if applied[version] != name {
			t.Fatalf("fresh migration %d = %q, want %q", version, applied[version], name)
		}
	}
	first.Close()
	if err := firstStore.Close(); err != nil {
		t.Fatalf("first store close: %v", err)
	}

	// The reopened database must still hold the secret and the Timer.
	secondStore := openTimerStore(t, path)
	defer secondStore.Close()
	reopenedSecrets, err := secrets.New(ctx, secondStore.DB(), []string{name})
	if err != nil {
		t.Fatalf("secrets.New after reopen: %v", err)
	}
	if value, ok := reopenedSecrets.Get(name); !ok || value != "secret-value-1234" {
		t.Fatalf("secret after reopen = %q, %v; want the stored value", value, ok)
	}
	second := storedManager(t, secondStore, nil, newRecorder(), nil)
	list := second.List()
	if len(list) != 1 || list[0].Label != "pasta" {
		t.Fatalf("timers after reopen = %+v, want the pasta timer", list)
	}
	if reopened := migrations(secondStore); len(reopened) != len(applied) {
		t.Fatalf("reopened migrations = %v, want the %d applied versions", reopened, len(applied))
	}
}

// TestTimerSurvivesRestart is the first acceptance criterion: a Timer set
// before a restart still goes off after it.
func TestTimerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")

	firstStore := openTimerStore(t, path)
	first := storedManager(t, firstStore, nil, newRecorder(), nil)
	started, err := first.Start("tea", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.ID == "" || started.Label != "tea" {
		t.Fatalf("Start returned %+v", started)
	}
	if got := countTimerRows(t, firstStore); got != 1 {
		t.Fatalf("persisted rows = %d, want 1", got)
	}
	first.Close()
	if err := firstStore.Close(); err != nil {
		t.Fatalf("first store close: %v", err)
	}

	// A new process opens the same file and recovers the Timer.
	secondStore := openTimerStore(t, path)
	defer secondStore.Close()
	rec := newRecorder()
	second := storedManager(t, secondStore, nil, rec, nil)
	if got := len(second.List()); got != 1 {
		t.Fatalf("recovered timers = %d, want 1", got)
	}
	if got := waitAlert(t, rec); got != "Tea timer is done." {
		t.Fatalf("alert = %q, want the labelled line", got)
	}
	if got := len(second.List()); got != 0 {
		t.Fatalf("timers after firing = %d, want 0", got)
	}
	if got := countTimerRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows after firing = %d, want 0", got)
	}
}

// TestNoDuplicateFireAfterRestart proves at-most-once: once a Timer fired and
// its row is gone, reopening the database cannot fire it again.
func TestNoDuplicateFireAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")

	firstStore := openTimerStore(t, path)
	first := storedManager(t, firstStore, nil, newRecorder(), nil)
	first.Start("coffee", 20*time.Millisecond)
	first.Close()
	firstStore.Close()

	// Open while the Timer is still due, let it fire, then restart again.
	secondStore := openTimerStore(t, path)
	secondRec := newRecorder()
	second := storedManager(t, secondStore, nil, secondRec, nil)
	waitAlert(t, secondRec)
	second.Close()
	secondStore.Close()

	thirdStore := openTimerStore(t, path)
	defer thirdStore.Close()
	thirdRec := newRecorder()
	storedManager(t, thirdStore, nil, thirdRec, nil)
	expectNoAlert(t, thirdRec, 200*time.Millisecond)
	if got := countTimerRows(t, thirdStore); got != 0 {
		t.Fatalf("persisted rows after duplicate check = %d, want 0", got)
	}
}

// TestOverdueGraceBoundary checks the exact downtime rule with an injected
// clock: exactly two minutes overdue fires now, one millisecond more becomes
// part of the summary.
func TestOverdueGraceBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	// Milliseconds are the stored precision; truncate so the boundary is exact.
	now := time.UnixMilli(time.Now().UnixMilli())

	firstStore := openTimerStore(t, path)
	insertSavedTimer(t, firstStore, "pasta", 10*time.Minute, now.Add(-2*time.Minute))
	insertSavedTimer(t, firstStore, "cake", 10*time.Minute, now.Add(-2*time.Minute-time.Millisecond))
	firstStore.Close()

	secondStore := openTimerStore(t, path)
	defer secondStore.Close()
	rec := newRecorder()
	announcementRec := newAnnouncementRecorder()
	queue := announce.New(announcementRec, slog.Default())
	defer queue.Close()

	storedManager(t, secondStore, func() time.Time { return now }, rec, queue)

	if got := waitAlert(t, rec); got != "Pasta timer is done." {
		t.Fatalf("grace alert = %q, want the pasta line", got)
	}
	if got := waitAnnouncement(t, announcementRec); got != "You missed a timer: cake." {
		t.Fatalf("summary = %q, want the single older timer", got)
	}
	expectNoAlert(t, rec, 100*time.Millisecond)
	if got := countTimerRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows = %d, want 0", got)
	}
}

// TestOldOverdueTimersBecomeOneSummaryAnnouncement covers the second downtime
// criterion: older Timers are summarised once, not fired one by one.
func TestOldOverdueTimersBecomeOneSummaryAnnouncement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.Now()

	firstStore := openTimerStore(t, path)
	insertSavedTimer(t, firstStore, "pasta", 10*time.Minute, now.Add(-40*time.Minute))
	insertSavedTimer(t, firstStore, "tea", 5*time.Minute, now.Add(-20*time.Minute))
	insertSavedTimer(t, firstStore, "", 30*time.Minute, now.Add(-3*time.Minute))
	firstStore.Close()

	secondStore := openTimerStore(t, path)
	defer secondStore.Close()
	rec := newRecorder()
	announcementRec := newAnnouncementRecorder()
	queue := announce.New(announcementRec, slog.Default())
	defer queue.Close()

	storedManager(t, secondStore, func() time.Time { return now }, rec, queue)

	if got := waitAnnouncement(t, announcementRec); got != "You missed 3 timers: pasta, tea, 30 minutes." {
		t.Fatalf("summary = %q", got)
	}
	// One summary only: the queue is drained and no second line appears.
	select {
	case extra := <-announcementRec.texts:
		t.Fatalf("second summary announcement: %q", extra)
	case <-time.After(100 * time.Millisecond):
	}
	expectNoAlert(t, rec, 100*time.Millisecond)
	if got := countTimerRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows = %d, want 0", got)
	}
}

// TestCancelIsPersistent proves a cancelled Timer does not come back.
func TestCancelIsPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")

	firstStore := openTimerStore(t, path)
	first := storedManager(t, firstStore, nil, newRecorder(), nil)
	first.Start("pasta", time.Hour)
	if cancelled, err := first.CancelLabel("pasta"); err != nil || cancelled != 1 {
		t.Fatalf("CancelLabel = %d, %v; want 1, nil", cancelled, err)
	}
	first.Close()
	firstStore.Close()

	secondStore := openTimerStore(t, path)
	defer secondStore.Close()
	rec := newRecorder()
	second := storedManager(t, secondStore, nil, rec, nil)
	if got := len(second.List()); got != 0 {
		t.Fatalf("recovered timers = %d after cancel, want 0", got)
	}
	if got := countTimerRows(t, secondStore); got != 0 {
		t.Fatalf("persisted rows = %d after cancel, want 0", got)
	}
	expectNoAlert(t, rec, 100*time.Millisecond)
}

// TestAttachStoreReadsRowsWrittenByAnotherHandle keeps the persistence seam
// honest: the Manager reads the table rather than an in-memory copy of it.
func TestAttachStoreReadsRowsWrittenByAnotherHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	now := time.Now()
	st := openTimerStore(t, path)
	defer st.Close()
	insertSavedTimer(t, st, "tea", time.Hour, now.Add(time.Hour))

	m := storedManager(t, st, func() time.Time { return now }, newRecorder(), nil)
	list := m.List()
	if len(list) != 1 || list[0].Label != "tea" {
		t.Fatalf("List = %+v, want the seeded tea timer", list)
	}
	if got, want := list[0].RemainingSeconds, 3600; got != want {
		t.Fatalf("RemainingSeconds = %d, want %d", got, want)
	}
}
