package alertstore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/alertstore"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// recoverOutcome is what the recovery goroutine reports back.
type recoverOutcome struct {
	events []alertstore.Event
	err    error
}

// TestRecoverWaitsForConcurrentWriter reproduces the CI failure (#336): a
// second connection holds the write lock while this process recovers. SQLite
// must wait inside the busy timeout instead of failing at once with
// "database is locked (5) (SQLITE_BUSY)".
func TestRecoverWaitsForConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	recoverWhileLocked(t, st.DB(), alertstore.New(st.DB()), path, "timer")
}

// TestBusyTimeoutAppliesToFreshAndNonWALFiles proves the connection pragmas are
// set both for a brand-new database and for one created outside the store with
// the rollback journal (a per-database WAL setting that must be upgraded).
func TestBusyTimeoutAppliesToFreshAndNonWALFiles(t *testing.T) {
	ctx := context.Background()
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	st, err := store.Open(ctx, fresh)
	if err != nil {
		t.Fatalf("store.Open fresh: %v", err)
	}
	assertPragmas(t, st.DB(), "fresh database")
	st.Close()

	// Create a rollback-journal database the way an older build or an external
	// tool would, then let the store reopen and adopt it.
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(legacy))
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		raw.Close()
		t.Fatalf("legacy journal mode: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE legacy_marker (id INTEGER PRIMARY KEY)`); err != nil {
		raw.Close()
		t.Fatalf("legacy table: %v", err)
	}
	raw.Close()

	st, err = store.Open(ctx, legacy)
	if err != nil {
		t.Fatalf("store.Open legacy: %v", err)
	}
	defer st.Close()
	assertPragmas(t, st.DB(), "non-WAL database")
	// The adopted file gets the same wait as a fresh one, not just the pragma
	// value: recovery under a second connection's write lock still completes.
	recoverWhileLocked(t, st.DB(), alertstore.New(st.DB()), legacy, "reminder")
}

// recoverWhileLocked opens a second handle on path, holds the write lock with
// an open transaction, and proves Recover waits for it instead of failing.
func recoverWhileLocked(t *testing.T, db *sql.DB, history *alertstore.Store, path, kind string) {
	t.Helper()
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_100_000)
	id := insertEvent(t, db, history, alertstore.Event{
		Kind: kind, SourceID: "held", Label: "held", FiresAt: at, FiredAt: at, State: alertstore.StateFired,
	})

	// A second handle on the same file is the other connection that held the
	// lock when the real reminder recovery failed.
	writer, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second store.Open: %v", err)
	}
	defer writer.Close()
	tx, err := writer.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE alert_events SET ended_by = 'held' WHERE id = ?`, id); err != nil {
		tx.Rollback()
		t.Fatalf("take the write lock: %v", err)
	}

	done := make(chan recoverOutcome, 1)
	go func() {
		events, err := history.Recover(ctx, at.Add(time.Minute))
		done <- recoverOutcome{events: events, err: err}
	}()

	// Recovery must still be blocked on the lock, not already failed while the
	// writer is open.
	select {
	case got := <-done:
		t.Fatalf("Recover returned while the write lock was held: events=%+v err=%v", got.events, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("release the write lock: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Recover after the lock was released: %v", got.err)
		}
		if len(got.events) != 1 || got.events[0].ID != id {
			t.Fatalf("recovered = %+v, want the held event closed", got.events)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recover did not finish after the lock was released")
	}
	if state := eventState(t, history, id); state != alertstore.StateMissed {
		t.Fatalf("state = %q, want missed", state)
	}
}

// assertPragmas checks the timeout the busy handler needs and the journal mode
// the store promises, reading them the way SQLite exposes them per connection.
func assertPragmas(t *testing.T, db *sql.DB, label string) {
	t.Helper()
	var timeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("%s: read busy_timeout: %v", label, err)
	}
	if timeout != 5000 {
		t.Fatalf("%s: busy_timeout = %d, want 5000", label, timeout)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("%s: read journal_mode: %v", label, err)
	}
	if mode != "wal" {
		t.Fatalf("%s: journal_mode = %q, want wal", label, mode)
	}
}
