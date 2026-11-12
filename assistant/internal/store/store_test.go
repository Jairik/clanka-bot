package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openStore opens the database at dir/assistant.db and closes it with the
// test. It is the public seam every persistence feature uses.
func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st, path
}

func TestOpenCreatesTheDatabaseWithOwnerOnlyPermissions(t *testing.T) {
	st, path := openStore(t)
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
		"greeting", `"hello"`, "2026-11-12T00:00:00Z"); err != nil {
		t.Fatalf("write settings row: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode = %o, want 600", got)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %o, want 700", got)
	}
	// WAL and SHM files carry the same data; SQLite copies the database mode,
	// and the test pins that so a driver change cannot silently widen them.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, entry := range entries {
		entryInfo, err := entry.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", entry.Name(), err)
		}
		if got := entryInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", entry.Name(), got)
		}
	}
}

func TestOpenTightensAnExistingDatabaseMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "assistant.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed database file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("seed database mode: %v", err)
	}

	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode = %o after Open, want 600", got)
	}
}

func TestMigrationsAreIdempotentAndKeepDataAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "assistant.db")
	ctx := context.Background()

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := first.DB().ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
		"greeting", `"hello"`, "2026-11-12T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	var applied int
	if err := first.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied == 0 {
		t.Fatal("schema_migrations is empty after Open")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// A restart opens the same file again: migrations must not rerun or fail,
	// and the stored row must still be there.
	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	var again int
	if err := second.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&again); err != nil {
		t.Fatalf("count migrations after reopen: %v", err)
	}
	if again != applied {
		t.Errorf("schema_migrations count = %d after reopen, want %d", again, applied)
	}
	var value string
	if err := second.DB().QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, "greeting").Scan(&value); err != nil {
		t.Fatalf("read setting after reopen: %v", err)
	}
	if value != `"hello"` {
		t.Errorf("setting = %q after reopen", value)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := store.Open(context.Background(), ""); err == nil {
		t.Fatal("Open succeeded with an empty path, want error")
	}
}
