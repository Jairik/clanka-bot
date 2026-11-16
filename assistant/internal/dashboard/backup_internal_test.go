package dashboard

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

const backupOwner = "owner@example.com"

// newBackupHandler wires the real store and an authenticated Dashboard so the
// backup route exercises the same VACUUM INTO and download path as production.
func newBackupHandler(t *testing.T) *Handler {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h, err := New(Options{
		Owner:        backupOwner,
		Store:        st,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProxyAllowed: func(ip net.IP) bool { return ip.IsLoopback() },
		Hosts:        []string{"pi.example.ts.net"},
		// An empty fixture avoids the embedded UI build; the backup route does
		// not serve the app.
		UI: fstest.MapFS{},
	})
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}
	return h
}

func backupReq(method, target, login string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	// A same-origin POST is what the Dashboard's own fetch sends; the same-origin
	// guard accepts it. The cross-site test overrides this.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	return req
}

func backupServe(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// backupRows reads the downloaded bytes as a SQLite database and returns the
// table names and the value of one known settings row.
func backupRows(t *testing.T, body []byte) (tables []string, marker string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write restored db: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table rows: %v", err)
	}
	if err := db.QueryRow("SELECT value FROM settings WHERE key='backup_marker'").Scan(&marker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return tables, marker
}

func TestBackupDownloadsAVisibleSQLiteCopy(t *testing.T) {
	h := newBackupHandler(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('backup_marker', 'backup_value', ?)`, now); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	rec := backupServe(h, backupReq(http.MethodPost, "/api/ui/backup", backupOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	// The download must be a consistent SQLite file with the seeded row.
	tables, marker := backupRows(t, rec.Body.Bytes())
	if marker != "backup_value" {
		t.Fatalf("marker = %q, want backup_value", marker)
	}
	for _, want := range []string{"settings", "secrets"} {
		if !contains(tables, want) {
			t.Errorf("restored db missing table %q; have %v", want, tables)
		}
	}
	// Attachments are never cached.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	// The filename carries the assistant-<ts>.db shape, never a request path.
	// The issue specifies the unquoted form: filename=assistant-<ts>.db.
	disp := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disp, "attachment; filename=assistant-") || !strings.HasSuffix(disp, ".db") {
		t.Errorf("Content-Disposition = %q", disp)
	}
	// A real, non-empty copy of the database.
	if n := rec.Body.Len(); n == 0 {
		t.Errorf("backup body is empty")
	}
}

func TestBackupRefusesAConcurrentSecondRequest(t *testing.T) {
	h := newBackupHandler(t)
	// Simulate an in-progress backup: the one-slot semaphore is already taken.
	h.backupLock <- struct{}{}
	defer func() { <-h.backupLock }()

	rec := backupServe(h, backupReq(http.MethodPost, "/api/ui/backup", backupOwner))
	if rec.Code != http.StatusConflict {
		t.Fatalf("concurrent backup = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBackupRemovesTheTempFile(t *testing.T) {
	h := newBackupHandler(t)
	// data.dir is the directory the live database lives in.
	dataDir := filepath.Dir(h.store.Path())
	rec := backupServe(h, backupReq(http.MethodPost, "/api/ui/backup", backupOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup = %d, want 200", rec.Code)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") && strings.HasSuffix(e.Name(), ".db") {
			t.Errorf("temp backup file left behind: %s", e.Name())
		}
	}
}

func TestBackupRequiresAuthentication(t *testing.T) {
	h := newBackupHandler(t)
	// No identity header: the existing identity guard refuses before the route.
	rec := backupServe(h, backupReq(http.MethodPost, "/api/ui/backup", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated backup = %d, want 403", rec.Code)
	}
}

func TestBackupRejectsCrossSiteRequests(t *testing.T) {
	h := newBackupHandler(t)
	// A browser cross-site POST carries Sec-Fetch-Site: cross-site, which the
	// same-origin guard rejects even with the right identity.
	req := backupReq(http.MethodPost, "/api/ui/backup", backupOwner)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := backupServe(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site backup = %d, want 403", rec.Code)
	}
}

func TestBackupRejectsGet(t *testing.T) {
	h := newBackupHandler(t)
	rec := backupServe(h, backupReq(http.MethodGet, "/api/ui/backup", backupOwner))
	// A GET does not match the POST route, so it is refused (405) rather than
	// served as a download.
	if rec.Code == http.StatusOK {
		t.Fatalf("GET backup should not download, got 200")
	}
}

func TestIsDiskFull(t *testing.T) {
	cases := map[string]bool{
		"sqlite: database or disk is full": true,
		"database or disk is full":         true,
		"disk I/O error":                   false,
		"backup failed":                    false,
		"":                                 false,
	}
	for msg, want := range cases {
		if got := isDiskFull(errString(msg)); got != want {
			t.Errorf("isDiskFull(%q) = %v, want %v", msg, got, want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func contains(items []string, target string) bool {
	for _, it := range items {
		if it == target {
			return true
		}
	}
	return false
}
