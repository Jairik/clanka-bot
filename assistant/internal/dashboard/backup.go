package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// backupLock serialises the one VACUUM INTO copy per handler. The database
// connection is already single, but the semaphore lets a second concurrent
// request learn it is busy (409) instead of queuing behind the copy.
func newBackupLock() chan struct{} { return make(chan struct{}, 1) }

// backupTag is the safe timestamp used in the temp path and the download
// filename. It is built from digits and letters only, so it cannot carry a path
// or a header injection.
func backupTag(now time.Time) string {
	return now.UTC().Format("20060102T150405Z")
}

// backup runs an authenticated, online SQLite backup and streams the copy to the
// caller. It is a POST (a state change and a large body) behind h.Mutating, so
// the existing identity and same-origin guards already apply: an
// unauthenticated or cross-site request never reaches this handler.
//
// The copy is made with VACUUM INTO on the existing single connection. That
// produces a consistent snapshot of the live database, including the WAL, into a
// fresh file under data.dir, so a backup never opens the file outside the
// assistant process. The temp file is removed on every return path.
func (h *Handler) backup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.store == nil {
		http.Error(w, "Backup is unavailable.", http.StatusServiceUnavailable)
		return
	}
	// Only one backup at a time. A second concurrent request gets 409.
	select {
	case h.backupLock <- struct{}{}:
		defer func() { <-h.backupLock }()
	default:
		http.Error(w, "A backup is already in progress.", http.StatusConflict)
		return
	}

	now := h.now()
	// data.dir is the directory holding the live database file; the temp copy
	// lives beside it so VACUUM INTO writes on the same volume that must hold
	// the second copy anyway.
	dataDir := filepath.Dir(h.store.Path())
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		h.logger.Error("backup random", "error", err.Error())
		http.Error(w, "Backup failed to start.", http.StatusInternalServerError)
		return
	}
	ts := backupTag(now)
	tempPath := filepath.Join(dataDir, fmt.Sprintf(".backup-%s-%s.db", ts, hex.EncodeToString(randBytes)))

	// Removed on every return: a successful stream, a VACUUM failure, or a
	// streaming error. The download must never leave the secrets-bearing copy
	// behind on disk.
	defer os.Remove(tempPath)

	// VACUUM INTO copies into a fresh file addressed by a single-quoted SQL
	// literal. Any embedded single quote is doubled so the literal cannot be
	// altered into a second statement. The path comes from data.dir, never the
	// request.
	escaped := strings.ReplaceAll(tempPath, "'", "''")
	if _, err := h.store.DB().ExecContext(r.Context(), fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		// A full data volume is the common failure for a second full copy;
		// report it clearly instead of as a generic 500.
		if isDiskFull(err) {
			http.Error(w, "Backup failed: the data volume is full. Free space and try again.", http.StatusInsufficientStorage)
			return
		}
		// Log the failure class only, never the path's contents.
		h.logger.Error("backup vacuum", "error", err.Error())
		http.Error(w, "Backup failed.", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(tempPath)
	if err != nil {
		h.logger.Error("backup open", "error", err.Error())
		http.Error(w, "Backup failed.", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.logger.Error("backup stat", "error", err.Error())
		http.Error(w, "Backup failed.", http.StatusInternalServerError)
		return
	}

	// Attachments are never cached and never carry the body into a log.
	name := "assistant-" + ts + ".db"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		h.logger.Error("backup stream", "error", err.Error())
	}
}

// isDiskFull reports whether err is SQLite's "database or disk is full", the
// usual outcome when VACUUM INTO cannot write the second copy.
func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "disk is full")
}
