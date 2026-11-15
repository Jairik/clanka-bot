// Trigram search index (#210). The durable history search used to be
// `instr(search, ?) > 0` over every retained row: newest-first paging could
// stop early, but a needle that is absent, which is the normal case while an
// owner narrows a query, had to read every haystack in the window before it
// could answer "no matches". The assistant runs SQLite on one connection
// (internal/store), so that scan also queued every other read and write behind
// it: a settings save, an alert fire, the next Activity insert.
//
// This file owns an FTS5 index over the same lowercased haystack, with the
// trigram tokenizer so the page keeps substring behavior ("weath" still finds
// "weather") that a debounced search-as-you-type box relies on. An
// external-content table keeps the text once, in activity; three triggers keep
// the index in sync with the writer, retention's trim UPDATE and the age and
// row-cap DELETEs.
//
// The index is created here rather than in a store migration on purpose:
// internal/store owns the tables, but a build without FTS5 must not fail the
// whole store at startup. When the index cannot be created, a search falls
// back to a bounded window of the newest rows (see shortSearchWindow) instead
// of the unbounded scan this issue is about.
package activity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	// minTrigramRunes is the shortest query the trigram index can answer. FTS5
	// indexes three-character sequences, so a one or two character query has no
	// index entry to look up and takes the bounded window fallback below.
	minTrigramRunes = 3
	// shortSearchWindow bounds that fallback, and any search when the index is
	// unavailable. It is wider than the in-memory ring (DefaultLimit) plus a
	// full page, so a short query still matches what the first page shows, and
	// it keeps one such request to a bounded read rather than the whole
	// retained window.
	shortSearchWindow = 200
	// searchBackfillBatch bounds one index write. A fresh process may have to
	// index a whole retained window (20,000 rows), so it does it in batches of
	// this many rows, one transaction each, resuming from a persisted cursor:
	// no single statement holds the one connection for the whole rebuild.
	searchBackfillBatch = 200
	// searchBackfillPause yields the database between batches so a waiting
	// request is served next instead of queueing behind a continuous rebuild.
	searchBackfillPause = 10 * time.Millisecond
)

// searchIndexStatements create the index, its triggers and the warmup cursor.
// Every statement is idempotent, so a restart, an upgrade from a build without
// the index, or a retry after a partial failure converges on the same schema.
var searchIndexStatements = []string{
	// External content: the haystack stays once, in activity, and content_rowid
	// ties an index entry to the activity id the page filters and orders by.
	`CREATE VIRTUAL TABLE IF NOT EXISTS activity_fts USING fts5(
		search, content='activity', content_rowid='id', tokenize='trigram')`,
	// Add() writes one row; the trigger indexes exactly that haystack, so a
	// completed request is searchable immediately.
	`CREATE TRIGGER IF NOT EXISTS activity_fts_insert AFTER INSERT ON activity BEGIN
		INSERT INTO activity_fts (rowid, search) VALUES (new.id, new.search);
	END`,
	// Retention's age and row-cap DELETEs drop their index entry with the row,
	// so a deleted row never matches again.
	`CREATE TRIGGER IF NOT EXISTS activity_fts_delete AFTER DELETE ON activity BEGIN
		INSERT INTO activity_fts (activity_fts, rowid, search) VALUES ('delete', old.id, old.search);
	END`,
	// A trim UPDATE rewrites the haystack to what the trimmed row still keeps;
	// external-content FTS5 needs the old value to retract its entries.
	`CREATE TRIGGER IF NOT EXISTS activity_fts_update AFTER UPDATE OF search ON activity BEGIN
		INSERT INTO activity_fts (activity_fts, rowid, search) VALUES ('delete', old.id, old.search);
		INSERT INTO activity_fts (rowid, search) VALUES (new.id, new.search);
	END`,
	// One row holds the warmup cursor: indexed_through is the newest activity id
	// already in the index, and complete says the rebuild finished. Both survive
	// a restart, so a second start neither duplicates index entries nor
	// re-tokenizes the window.
	`CREATE TABLE IF NOT EXISTS activity_search_state (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		indexed_through INTEGER NOT NULL DEFAULT 0,
		complete        INTEGER NOT NULL DEFAULT 0
	)`,
	`INSERT OR IGNORE INTO activity_search_state (id, indexed_through, complete) VALUES (1, 0, 0)`,
}

// SearchIndexState is the persisted position of the trigram index warmup.
type SearchIndexState struct {
	// IndexedThrough is the newest activity id the rebuild has indexed. Rows
	// above it are still answered by the bounded fallback branch of a search.
	IndexedThrough int64
	// Complete is true once every retained row was indexed, after which the
	// triggers alone keep the index current.
	Complete bool
}

// WarmSearchIndex creates the index when needed and indexes one bounded batch
// of existing rows. It is idempotent and resumable: the cursor is persisted in
// the same transaction as the batch, so a restart continues where the last
// batch stopped, and re-indexing a row replaces its entry rather than adding a
// second one. A nil History, or one whose window is already indexed, does no
// work and reports Complete.
func (h *History) WarmSearchIndex(ctx context.Context) (SearchIndexState, error) {
	if h == nil {
		return SearchIndexState{Complete: true}, nil
	}
	if err := h.ensureSearchIndex(ctx); err != nil {
		return SearchIndexState{}, err
	}
	// One warmup at a time: the background loop and a caller that wants a warm
	// index for a test must not index the same batch twice.
	h.warmMu.Lock()
	defer h.warmMu.Unlock()

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state SearchIndexState
	if err := tx.QueryRowContext(ctx,
		`SELECT indexed_through, complete FROM activity_search_state WHERE id = 1`).
		Scan(&state.IndexedThrough, &state.Complete); err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: read state: %w", err)
	}
	if state.Complete {
		return state, nil
	}

	// Reading the batch and indexing it share one transaction, so retention
	// cannot trim a haystack between the read and the index write (which would
	// leave the dropped text searchable), and the cursor cannot lag a batch.
	type row struct {
		id     int64
		search string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, search FROM activity WHERE id > ? ORDER BY id LIMIT ?`,
		state.IndexedThrough, searchBackfillBatch)
	if err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: read batch: %w", err)
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.search); err != nil {
			rows.Close()
			return SearchIndexState{}, fmt.Errorf("activity search index: scan batch: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SearchIndexState{}, fmt.Errorf("activity search index: read batch: %w", err)
	}
	rows.Close()

	for _, r := range batch {
		// OR REPLACE makes a repeated batch harmless: the rowid is the activity
		// id, so a second attempt replaces the entry instead of duplicating it.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO activity_fts (rowid, search) VALUES (?, ?)`, r.id, r.search); err != nil {
			return SearchIndexState{}, fmt.Errorf("activity search index: index row: %w", err)
		}
	}
	if len(batch) > 0 {
		state.IndexedThrough = batch[len(batch)-1].id
	}
	// A short batch means the window is exhausted; a long one means another
	// batch is due.
	state.Complete = len(batch) < searchBackfillBatch
	if _, err := tx.ExecContext(ctx,
		`UPDATE activity_search_state SET indexed_through = ?, complete = ? WHERE id = 1`,
		state.IndexedThrough, state.Complete); err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: save state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: commit: %w", err)
	}
	return state, nil
}

// ensureSearchIndex applies the idempotent schema once and remembers the
// outcome. A failure is remembered too: a build without the trigram tokenizer
// should degrade to the bounded window, not retry DDL on every search.
func (h *History) ensureSearchIndex(ctx context.Context) error {
	h.schemaMu.Lock()
	defer h.schemaMu.Unlock()
	if h.schemaReady {
		return nil
	}
	if h.schemaErr != nil {
		return h.schemaErr
	}
	for _, statement := range searchIndexStatements {
		if _, err := h.db.ExecContext(ctx, statement); err != nil {
			h.schemaErr = fmt.Errorf("activity search index: %w", err)
			if !isClosedDatabase(err) {
				slog.Default().Warn("activity search index unavailable; searches use the bounded window",
					"error", err.Error())
			}
			return h.schemaErr
		}
	}
	h.schemaReady = true
	return nil
}

// warmSearchIndex rebuilds the index for an upgraded database in the
// background. Startup must not wait for a 20,000-row tokenization, and a
// search answers correctly meanwhile: see searchCondition. A failure leaves
// the cursor where it stopped and logs one warning; searches then stay on the
// bounded window, which is slower but still correct, until the next restart.
func (h *History) warmSearchIndex() {
	for {
		state, err := h.WarmSearchIndex(context.Background())
		if err != nil {
			if !isClosedDatabase(err) {
				slog.Default().Warn("activity search index warmup", "error", err.Error())
			}
			return
		}
		if state.Complete {
			return
		}
		time.Sleep(searchBackfillPause)
	}
}

// isClosedDatabase reports the shutdown race between a warmup batch and
// Close, which is not worth a log line.
func isClosedDatabase(err error) bool {
	return errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "database is closed")
}

// searchCondition is the WHERE clause and arguments for one free-text query.
// The trigram index answers it as an id set, so a no-match search reads the
// index, not the retained window.
func (h *History) searchCondition(ctx context.Context, needle string) (string, []any, error) {
	state, err := h.searchIndexState(ctx)
	if err != nil {
		return "", nil, err
	}
	phrase := ftsPhrase(needle)
	if state.Complete {
		return `id IN (SELECT rowid FROM activity_fts WHERE activity_fts MATCH ?)`, []any{phrase}, nil
	}
	// The rebuild runs oldest first, so the only rows missing from the index
	// are above its cursor. That tail is bounded by the un-indexed part of the
	// window and shrinks to nothing, so a search is complete at every moment
	// instead of silently ignoring rows it has not indexed yet.
	return `(id IN (SELECT rowid FROM activity_fts WHERE activity_fts MATCH ?)
		OR (id > ? AND instr(search, ?) > 0))`,
		[]any{phrase, state.IndexedThrough, needle}, nil
}

// searchIndexState reads the persisted warmup cursor. It is one primary-key
// lookup in a one-row table.
func (h *History) searchIndexState(ctx context.Context) (SearchIndexState, error) {
	var state SearchIndexState
	if err := h.db.QueryRowContext(ctx,
		`SELECT indexed_through, complete FROM activity_search_state WHERE id = 1`).
		Scan(&state.IndexedThrough, &state.Complete); err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: read state: %w", err)
	}
	return state, nil
}

// ftsPhrase quotes one needle as an FTS5 string, doubling an embedded quote, so
// punctuation in a user query can never be parsed as FTS5 syntax.
func ftsPhrase(needle string) string {
	return `"` + strings.ReplaceAll(needle, `"`, `""`) + `"`
}

// windowedSearch reports whether a query has to use the bounded newest-window
// fallback instead of the index. One and two character queries have no trigram
// to look up; the index may also be missing (an older build, or a schema write
// that failed).
func (h *History) windowedSearch(ctx context.Context, needle string) bool {
	if len([]rune(needle)) < minTrigramRunes {
		return true
	}
	return h.ensureSearchIndex(ctx) != nil
}

// pageByWindow answers a search from the newest shortSearchWindow rows only.
// The window is materialized first, so the LIMIT bounds the rows read rather
// than the rows returned: a needle that is absent costs one bounded read
// instead of a scan of the retained window.
func (h *History) pageByWindow(ctx context.Context, conditions []string, args []any, needle string, limit int) (Page, error) {
	statement := `WITH window AS MATERIALIZED (
		SELECT id, at, search FROM activity`
	if len(conditions) > 0 {
		statement += " WHERE " + strings.Join(conditions, " AND ")
	}
	statement += `
		ORDER BY at DESC, id DESC LIMIT ?)
		SELECT id FROM window WHERE instr(search, ?) > 0
		ORDER BY at DESC, id DESC LIMIT ?`
	windowArgs := make([]any, 0, len(args)+3)
	windowArgs = append(windowArgs, args...)
	windowArgs = append(windowArgs, shortSearchWindow, needle, limit+1)

	rows, err := h.db.QueryContext(ctx, statement, windowArgs...)
	if err != nil {
		return Page{}, fmt.Errorf("activity history: query: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Page{}, fmt.Errorf("activity history: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Page{}, fmt.Errorf("activity history: read: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		return Page{}, nil
	}
	return h.pageByID(ctx, ids, limit)
}

// pageByID loads the rows behind an ordered id list and keeps that order, so a
// windowed page and an indexed page agree on newest-first order.
func (h *History) pageByID(ctx context.Context, ids []int64, limit int) (Page, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM activity WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return Page{}, fmt.Errorf("activity history: query: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]scanResult, len(ids))
	for rows.Next() {
		scanned, err := scanEntry(rows)
		if err != nil {
			return Page{}, err
		}
		byID[scanned.id] = scanned
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("activity history: read: %w", err)
	}

	var entries []Entry
	var cursors []string
	for _, id := range ids {
		scanned, ok := byID[id]
		if !ok {
			// A row deleted between the window read and this read is simply
			// gone, the same way retention can remove a row mid-page.
			continue
		}
		scanned.entry.Cursor = scanned.cursor
		entries = append(entries, scanned.entry)
		cursors = append(cursors, scanned.cursor)
	}
	page := Page{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.Next = cursors[limit-1]
	}
	return page, nil
}
