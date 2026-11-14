// Package store owns the assistant's single SQLite database (ADR-0003). Only
// this process opens the file; the Dashboard and other services reach the
// state through the assistant's API. Migrations run at startup and are
// idempotent, so a restart or a rebuild keeps the same data volume.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite keeps the arm64 image cgo-free.
	_ "modernc.org/sqlite"
)

// Store is the one database handle.
type Store struct {
	db   *sql.DB
	path string
}

// migration is one forward-only schema step. Add a newer version for each
// ticket that needs new tables; applied versions are recorded and never rerun.
type migration struct {
	version    int
	name       string
	statements []string
}

// migrations is the schema history. Version 1 is the settings overrides table
// shared by the Dashboard, voice Tools and the tool policy. Version 2 adds the
// write-only secrets table: values never take a settings key, so the settings
// API cannot list or return them. Version 3 holds the Scheduled alerts (Timer
// rows today; Alarm and Reminder rows reuse the shape), so they survive a
// restart (ADR-0003). Version 4 indexes the Sound library: built-in sounds and
// WAV files in sounds/ with their name, duration and first-added time. Cue role
// assignments are ordinary settings rows, so they share this database too.
// Version 5 is the alert history: a fired Timer deletes its pending row in the
// same transaction that opens a history row, so an at-most-once fire still
// leaves a durable fired/ringing/stopped/missed record for crash recovery and
// the later notification hook (#62). Version 6 indexes custom Pocket TTS voice
// states in voices/: the file, its size/mtime for replacement detection, the
// model revision it was validated against, staleness, the last validation
// error and the optional consent note. The active voice and rate are ordinary
// settings rows. Version 7 holds Alarms: their clock time, repeat rule, next
// occurrence and an optional pending snooze. A one-off Alarm keeps its row
// with next_fires_at = 0 until the Ring outcome is known, so a snooze survives
// a restart; a repeating Alarm advances next_fires_at in the same transaction
// that opens its history row. sound, volume and ramp_ms are reserved for #57's
// per-Alarm overrides and stay NULL/empty until then. Version 8 holds Reminders:
// a message and the duration or clock time it is due. Version 9 holds Personas:
// a named bundle of voice, personality prompt, Agent Filler and Unavailable
// wording. The active Persona is an ordinary settings row, and selecting one
// writes the bundle through the settings service in one transaction. Version 10
// holds the personal Notes and the fixed shopping and to-do Lists (#32); the
// row text is user content, so it never reaches a log. Version 11 is the
// durable Activity history (#96): every completed request, with search text
// maintained by the writer and indexed on at/source/outcome so paging is one
// bounded range scan. Rows are always written with 0600 database permissions
// and never leave this process. Version 12 adds the optional completion audio
// on Timers and Alarms. Version 13 is the per-Persona phrase book: one short
// spoken confirmation per Persona and overridable Tool, with the shipped
// terse default kept in code. The versions stay contiguous in either merge
// order.
var migrations = []migration{
	{
		version: 1,
		name:    "settings",
		statements: []string{
			`CREATE TABLE settings (
				key        TEXT PRIMARY KEY,
				value      TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
		},
	},
	{
		version: 2,
		name:    "secrets",
		statements: []string{
			`CREATE TABLE secrets (
				name       TEXT PRIMARY KEY,
				value      TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
		},
	},
	{
		version: 3,
		name:    "timers",
		statements: []string{
			`CREATE TABLE timers (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				label       TEXT NOT NULL DEFAULT '',
				duration_ms INTEGER NOT NULL,
				fires_at    INTEGER NOT NULL,
				created_at  INTEGER NOT NULL
			)`,
			`CREATE INDEX timers_fires_at ON timers (fires_at)`,
		},
	},
	{
		version: 4,
		name:    "sounds",
		statements: []string{
			// path is relative to the library (data.dir/sounds) and empty for a
			// built-in. added_at is the first time the name was indexed, so a
			// rescan does not reorder the library.
			`CREATE TABLE sounds (
				name        TEXT PRIMARY KEY,
				path        TEXT NOT NULL DEFAULT '',
				builtin     INTEGER NOT NULL DEFAULT 0,
				duration_ms INTEGER NOT NULL,
				added_at    TEXT NOT NULL
			)`,
		},
	},
	{
		version: 5,
		name:    "alert_events",
		statements: []string{
			// source_id is the scheduler's id before its pending row was
			// deleted. ended_at is NULL until a terminal state is written.
			`CREATE TABLE alert_events (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				kind        TEXT NOT NULL,
				source_id   TEXT NOT NULL DEFAULT '',
				label       TEXT NOT NULL DEFAULT '',
				duration_ms INTEGER NOT NULL,
				fires_at    INTEGER NOT NULL,
				fired_at    INTEGER NOT NULL,
				state       TEXT NOT NULL,
				ended_at    INTEGER,
				ended_by    TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX alert_events_state ON alert_events (state)`,
			`CREATE INDEX alert_events_fired_at ON alert_events (fired_at)`,
		},
	},
	{
		version: 6,
		name:    "voices",
		statements: []string{
			// file is relative to the voices library. added_at and mod_time are
			// Unix nanoseconds; validated_at is RFC3339. stale is set when TTS
			// starts on a different model revision than the one recorded here.
			`CREATE TABLE voices (
				name               TEXT PRIMARY KEY,
				file               TEXT NOT NULL,
				size               INTEGER NOT NULL,
				mod_time           INTEGER NOT NULL,
				added_at           INTEGER NOT NULL,
				validated_revision TEXT NOT NULL DEFAULT '',
				validated_at       TEXT NOT NULL DEFAULT '',
				stale              INTEGER NOT NULL DEFAULT 0,
				last_error         TEXT NOT NULL DEFAULT '',
				note               TEXT NOT NULL DEFAULT ''
			)`,
		},
	},
	{
		version: 7,
		name:    "alarms",
		statements: []string{
			// hour and minute are the spoken local clock time. schedule is
			// once/daily/weekdays/weekends/chosen; weekdays is a Sunday-first
			// bitmask used by chosen. next_fires_at is a UTC instant; 0 means a
			// one-off already fired and is waiting for its Ring outcome.
			// snoozed_until is NULL unless a snooze is pending.
			`CREATE TABLE alarms (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				label         TEXT NOT NULL DEFAULT '',
				hour          INTEGER NOT NULL,
				minute        INTEGER NOT NULL,
				schedule      TEXT NOT NULL,
				weekdays      INTEGER NOT NULL DEFAULT 0,
				next_fires_at INTEGER NOT NULL,
				created_at    INTEGER NOT NULL,
				snoozed_until INTEGER,
				sound         TEXT NOT NULL DEFAULT '',
				volume        INTEGER,
				ramp_ms       INTEGER
			)`,
			`CREATE INDEX alarms_next_fires_at ON alarms (next_fires_at)`,
		},
	},
	{
		version: 8,
		name:    "reminders",
		statements: []string{
			// message is the thing to say once when the Reminder is due. kind is
			// duration or clock; duration_ms is zero for a clock time. The row is
			// deleted in the same transaction that opens its alert history row.
			`CREATE TABLE reminders (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				message     TEXT NOT NULL,
				kind        TEXT NOT NULL DEFAULT 'duration',
				duration_ms INTEGER NOT NULL DEFAULT 0,
				fires_at    INTEGER NOT NULL,
				created_at  INTEGER NOT NULL
			)`,
			`CREATE INDEX reminders_fires_at ON reminders (fires_at)`,
		},
	},
	{
		version: 9,
		name:    "personas",
		statements: []string{
			// name is the spoken key; the NOCASE collation rejects a case-only
			// duplicate. voice names a library entry and is re-checked for
			// readiness on every selection. personality is the only field that
			// reaches the Agent path, inside the one labelled system part.
			// created_at and updated_at are RFC3339.
			`CREATE TABLE personas (
				name        TEXT PRIMARY KEY COLLATE NOCASE,
				voice       TEXT NOT NULL,
				personality TEXT NOT NULL,
				filler      TEXT NOT NULL,
				unavailable TEXT NOT NULL,
				created_at  TEXT NOT NULL,
				updated_at  TEXT NOT NULL
			)`,
		},
	},
	{
		version: 10,
		name:    "notes_and_lists",
		statements: []string{
			// text is user content. embedding is the MiniLM vector captured
			// when the note was written, as little-endian float32; NULL means
			// the embed service was unavailable and recall falls back to exact
			// substring matches for that note. created_at and updated_at are
			// Unix milliseconds so ordering never depends on the database
			// locale.
			`CREATE TABLE notes (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				text       TEXT NOT NULL,
				embedding  BLOB,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX notes_updated_at ON notes (updated_at)`,
			// list_key is one of the fixed List keys (shopping, to-do); a
			// CHECK keeps a stray write from creating a third list by voice or
			// over the API. done is 0 or 1; position follows created_at, id.
			`CREATE TABLE list_items (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				list_key   TEXT NOT NULL CHECK (list_key IN ('shopping', 'to-do')),
				text       TEXT NOT NULL,
				done       INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX list_items_list_key ON list_items (list_key, done, id)`,
		},
	},
	{
		version: 11,
		name:    "activity_history",
		statements: []string{
			// One row per completed request. at is Unix milliseconds so paging
			// and retention never depend on the database locale. stages,
			// checkpoints and calls are compact JSON copies of the display
			// fields; search is the lowercased haystack the dashboard queries,
			// maintained by the writer so a read never builds it. summary is 1
			// once retention has trimmed the long text. request_id is the
			// pipeline id, not unique: a replayed id keeps both rows.
			`CREATE TABLE activity (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				request_id  TEXT NOT NULL DEFAULT '',
				at          INTEGER NOT NULL,
				source      TEXT NOT NULL DEFAULT '',
				path        TEXT NOT NULL DEFAULT '',
				tool        TEXT NOT NULL DEFAULT '',
				outcome     TEXT NOT NULL DEFAULT '',
				error       TEXT NOT NULL DEFAULT '',
				transcript  TEXT NOT NULL DEFAULT '',
				reply       TEXT NOT NULL DEFAULT '',
				local_only  INTEGER NOT NULL DEFAULT 0,
				response_ms INTEGER,
				stages      TEXT NOT NULL DEFAULT '[]',
				checkpoints TEXT NOT NULL DEFAULT '[]',
				calls       TEXT NOT NULL DEFAULT '[]',
				search      TEXT NOT NULL DEFAULT '',
				summary     INTEGER NOT NULL DEFAULT 0
			)`,
			// Newest-first paging, the source and problems filters, and the
			// age cutoff are each a bounded index range. There is no index on
			// search: a full-text prefix index would be written on every
			// request, and the query is bounded by the at range instead.
			`CREATE INDEX activity_at ON activity (at DESC, id DESC)`,
			`CREATE INDEX activity_source_at ON activity (source, at DESC)`,
			`CREATE INDEX activity_outcome_at ON activity (outcome, at DESC)`,
		},
	},
	{version: 12, name: "completion_audio", statements: []string{
		// Empty media preserves the existing Ring defaults for saved alerts.
		`ALTER TABLE timers ADD COLUMN audio TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE alarms ADD COLUMN audio TEXT NOT NULL DEFAULT ''`,
	}},
	{version: 13, name: "persona_phrases", statements: []string{
		// One short spoken confirmation per Persona and overridable Tool.
		// persona is the folded lookup key (Unicode lower-case), so the row
		// matches the Persona library's case-insensitive names for non-ASCII
		// spellings too. A Persona edit never renames the row. source is
		// "manual" or "agent" so the page can show where the text came from.
		`CREATE TABLE persona_phrases (
			persona    TEXT NOT NULL COLLATE NOCASE,
			tool       TEXT NOT NULL,
			phrase     TEXT NOT NULL,
			source     TEXT NOT NULL DEFAULT 'manual',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (persona, tool)
		)`,
	}},
}

// Open opens (creating if needed) the database at path, applies pending
// migrations, and returns the handle. A new file is 0600 and its parent
// directory 0700; an existing file with looser permissions is tightened.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	// Create the file ourselves so SQLite cannot create it with the process
	// umask (usually 0644), then tighten a pre-existing file from an older run.
	file, err := os.OpenFile(absolute, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create %s: %w", absolute, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("store: close %s: %w", absolute, err)
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		return nil, fmt.Errorf("store: set %s permissions: %w", absolute, err)
	}

	db, err := sql.Open("sqlite", dsn(absolute))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", absolute, err)
	}
	// One connection keeps SQLite writes serialised and avoids SQLITE_BUSY
	// between the settings, secrets and alert writers. Traffic is tiny.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect %s: %w", absolute, err)
	}

	st := &Store{db: db, path: absolute}
	if err := st.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// dsn builds the SQLite URI: WAL for restart-safe writes, a busy timeout for
// the single writer, and foreign keys for later tables.
func dsn(path string) string {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := uri.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(NORMAL)")
	uri.RawQuery = query.Encode()
	return uri.String()
}

// migrate creates the ledger, then applies each unapplied version in its own
// transaction. Rerunning Open therefore does nothing on an up-to-date file.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create migration ledger: %w", err)
	}

	applied := make(map[int]bool, len(migrations))
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read migration ledger: %w", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return fmt.Errorf("store: read migration version: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: read migration ledger: %w", err)
	}
	rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin migration %d (%s): %w", m.version, m.name, err)
		}
		for _, statement := range m.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				tx.Rollback()
				return fmt.Errorf("store: migration %d (%s): %w", m.version, m.name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %d (%s): %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// Path returns the absolute database file path.
func (s *Store) Path() string { return s.path }

// DB exposes the handle for the small packages that own a table (settings,
// secrets, alerts). It must not be passed outside this process.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database. It is safe to call once at shutdown.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
