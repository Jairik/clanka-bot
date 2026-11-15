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
// "weather") that a debounced search-as-you-type box relies on.
//
// Two design decisions are load-bearing:
//
//   - The FTS table carries its own content and no triggers write it. An
//     external-content table only accepts the special 'delete' command, and
//     that command raises SQLITE_CORRUPT for a rowid the index does not hold
//     yet. During the one-time rebuild of an upgraded database, retention
//     (Prune) deletes rows the rebuild has not reached, and a trigger would
//     retract an entry that never existed; the whole prune statement then
//     fails with "database disk image is malformed". A normal FTS5 table is
//     maintained explicitly from the writer paths instead: Add, the trim
//     UPDATE and the age/row-cap DELETEs each retract and re-insert the same
//     rowid, and a DELETE for a rowid that is not indexed is a harmless no-op.
//     A crash therefore cannot leave a trigger and an index disagreeing: there
//     are no triggers, and every maintenance step is idempotent (delete then
//     insert inside one transaction with the activity row).
//
//   - Merging is off the insert path. FTS5's default automerge merges segments
//     when a transaction commits, on the goroutine that also holds the only
//     database connection: measured at 20,000 rows, the 99th percentile of a
//     history insert was 219 us before the index and 48.7 ms with the default
//     policy. With automerge(0) and a high crisismerge the insert path does no
//     merge work at all; Prune runs a bounded 'merge' from the maintenance
//     path, and a full 'optimize' at most every searchOptimizeInterval.
//
// The index is created here rather than in a store migration on purpose:
// internal/store owns the tables, but a build without FTS5 must not fail the
// whole store at startup. When the index cannot be created, a search falls
// back to a bounded window of the newest rows and says so on the page (see
// ShortSearchWindow and SearchWindow).
package activity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	// minTrigramRunes is the shortest query the trigram index can answer. FTS5
	// indexes three-character sequences, so a one or two character query has no
	// index entry to look up and takes the bounded window fallback below.
	minTrigramRunes = 3
	// ShortSearchWindow bounds the fallback for a one or two character query,
	// and any search when the index is unavailable. It is wider than the
	// in-memory ring (DefaultLimit) plus a full page, so a short query still
	// matches what the first page shows, and it keeps one such request to a
	// bounded read rather than the whole retained window. The page states this
	// bound, so the copy and the code share one number.
	ShortSearchWindow = 200
	// searchBackfillBatch bounds one rebuild step. A fresh process may have to
	// index a whole retained window (20,000 rows), so it does it in batches of
	// this many rows, one transaction each, resuming from a persisted cursor:
	// no single statement holds the one connection for the whole rebuild.
	searchBackfillBatch = 200
	// searchBackfillPause yields the database between batches so a waiting
	// request is served next instead of queueing behind a continuous rebuild.
	searchBackfillPause = 10 * time.Millisecond

	// searchPageChunk bounds one indexed read. The page query reads this many
	// matching rowids (newest id first), joins only their id and time, and
	// sorts those: a needle that matches the whole window costs one bounded
	// chunk, not a materialization of 20,000 matches and all sixteen columns.
	searchPageChunk = 200
	// searchWindowSlack is how many rows beyond a full page the newest-row
	// probe reads. The probe answers the common case (a needle that matches
	// most of the window) at the same cost as the scan it replaces; only when
	// it cannot fill a page does the index run.
	searchWindowSlack = 8

	// searchMergePages bounds one maintenance merge, in pages of index data.
	// It runs from Prune, never from Add, so a merge burst cannot land on the
	// goroutine that serves a request. The bound covers an hour of writes with
	// room to spare: at the 4 KB search cap a busy hour is about 120 rows, or
	// 30 pages of index text.
	searchMergePages = 128
	// searchVacuumPages bounds one incremental-vacuum pass. It only returns
	// pages when the database was created (or VACUUMed) with
	// auto_vacuum=INCREMENTAL; otherwise it is a cheap no-op.
	searchVacuumPages = 2000
	// searchOptimizeInterval is the shortest gap between full index
	// compactions. A trim rewrites a row's postings, so the index accumulates
	// dead space that 'merge' only partly reclaims; 'optimize' rewrites it to
	// the live text, after which SQLite reuses the freed pages.
	searchOptimizeInterval = 6 * time.Hour
	// searchSchemaVersion identifies this index design in
	// activity_search_state. A database written by another design (the
	// external-content table with triggers, or a hand-edited state row) is
	// rebuilt instead of trusted.
	searchSchemaVersion = 2

	// searchAutomerge and searchCrisisMerge are the merge policy, set when the
	// index schema is created. Zero automerge keeps all merge work off the
	// insert path; the high crisis threshold is only a safety valve for a
	// process whose Prune never runs, and a bounded 'merge' in the maintenance
	// path keeps the segment count low in normal operation.
	searchAutomerge   = 0
	searchCrisisMerge = 512
)

// SearchWindow says why a text search only covered part of the retained
// history. The zero value is a complete search.
type SearchWindow string

const (
	// SearchWindowNone is a complete answer over every retained row.
	SearchWindowNone SearchWindow = ""
	// SearchWindowShort is a one or two character query: a trigram index has no
	// entry to look up, so only the newest ShortSearchWindow rows were read.
	SearchWindowShort SearchWindow = "short"
	// SearchWindowIndexUnavailable means the index could not be created in this
	// build, so the same bounded window answered every query.
	SearchWindowIndexUnavailable SearchWindow = "index_unavailable"
)

// searchStateDDL and searchStateColumns create the one-row state table. It
// holds the rebuild cursor, a completion latch, the schema version and the
// time of the last full compaction. A state table written by the previous
// design lacks the last two columns, so ensureSearchIndex adds them.
const searchStateDDL = `CREATE TABLE IF NOT EXISTS activity_search_state (
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	indexed_through INTEGER NOT NULL DEFAULT 0,
	complete        INTEGER NOT NULL DEFAULT 0,
	version         INTEGER NOT NULL DEFAULT 0,
	maintained_at   INTEGER NOT NULL DEFAULT 0
)`

// searchTableDDL creates the index. It is a normal (content-bearing) FTS5
// table: the text is stored twice, in activity and in the index, which is the
// price of a DELETE that cannot corrupt an index it has not caught up with yet.
const searchTableDDL = `CREATE VIRTUAL TABLE IF NOT EXISTS activity_fts USING fts5(search, tokenize='trigram')`

// searchTriggers are the three sync triggers the previous design created. They
// are dropped on sight: an upgrade from that design must never leave a trigger
// that can retract an index entry the rebuild does not hold.
var searchTriggers = []string{
	`DROP TRIGGER IF EXISTS activity_fts_insert`,
	`DROP TRIGGER IF EXISTS activity_fts_delete`,
	`DROP TRIGGER IF EXISTS activity_fts_update`,
}

// SearchIndexState is the persisted position of the trigram index rebuild.
type SearchIndexState struct {
	// IndexedThrough is the newest activity id the rebuild has indexed. Rows
	// above it are answered by the bounded fallback branch of a search until
	// the rebuild reaches them.
	IndexedThrough int64
	// Complete is true once every retained row was indexed, after which the
	// write paths alone keep the index current.
	Complete bool
}

// searchChunkSQL reads one bounded candidate set, newest id first. The rowid
// bound pages through older candidates without re-reading the ones already
// seen; the FTS5 plan is a reverse scan of the match list, not a sort.
const searchChunkSQL = `SELECT rowid FROM activity_fts
	WHERE activity_fts MATCH ? AND rowid < ?
	ORDER BY rowid DESC LIMIT ?`

// WarmSearchIndex creates the index when needed and indexes one bounded batch
// of existing rows. It is idempotent and resumable: the cursor is persisted in
// the same transaction as the batch, and a batch first retracts its id range
// and then re-indexes the rows that still exist, so a retry replaces entries
// instead of duplicating them. A nil History, or one whose window is already
// indexed, does no work and reports Complete.
func (h *History) WarmSearchIndex(ctx context.Context) (SearchIndexState, error) {
	if h == nil {
		return SearchIndexState{Complete: true}, nil
	}
	if err := h.ensureSearchIndex(ctx); err != nil {
		return SearchIndexState{}, err
	}
	// One rebuild at a time: the background loop and a caller that wants a warm
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
	// cannot trim a haystack between the read and the index write, and the
	// cursor cannot lag a batch.
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

	if len(batch) > 0 {
		upper := batch[len(batch)-1].id
		// Retract the whole id range first. A row deleted by retention after an
		// earlier attempt leaves a stale entry that this removes; rows that
		// still exist are re-inserted below. Running a committed batch twice
		// therefore leaves the same index, not a duplicate.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM activity_fts WHERE rowid > ? AND rowid <= ?`,
			state.IndexedThrough, upper); err != nil {
			return SearchIndexState{}, fmt.Errorf("activity search index: clear batch: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO activity_fts (rowid, search)
			SELECT id, search FROM activity WHERE id > ? AND id <= ? ORDER BY id`,
			state.IndexedThrough, upper); err != nil {
			return SearchIndexState{}, fmt.Errorf("activity search index: index batch: %w", err)
		}
		state.IndexedThrough = upper
	}
	// A short batch means the window is exhausted; a long one means another
	// batch is due.
	state.Complete = len(batch) < searchBackfillBatch
	freshlyComplete := state.Complete
	if _, err := tx.ExecContext(ctx,
		`UPDATE activity_search_state SET indexed_through = ?, complete = ? WHERE id = 1`,
		state.IndexedThrough, state.Complete); err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: save state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SearchIndexState{}, fmt.Errorf("activity search index: commit: %w", err)
	}
	if freshlyComplete {
		// One compaction at the end of a rebuild: the batches above created one
		// segment each, and an unmerged 20,000-row index multiplies the cost of
		// every search. This runs on the rebuild goroutine, never on a request.
		h.compactSearchIndex(ctx, true)
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
	if err := h.applySearchSchema(ctx); err != nil {
		h.schemaErr = fmt.Errorf("activity search index: %w", err)
		if !isClosedDatabase(err) {
			slog.Default().Warn("activity search index unavailable; searches use the bounded window",
				"error", err.Error())
		}
		return h.schemaErr
	}
	h.schemaReady = true
	return nil
}

// applySearchSchema converges any database on this index design. Every
// statement is idempotent, so a restart, an upgrade from a build without the
// index, or a retry after a partial failure converges on the same schema.
func (h *History) applySearchSchema(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, searchStateDDL); err != nil {
		return fmt.Errorf("state table: %w", err)
	}
	// A state table written before this design has no version or maintained_at.
	for _, column := range []struct{ name, ddl string }{
		{"version", `ALTER TABLE activity_search_state ADD COLUMN version INTEGER NOT NULL DEFAULT 0`},
		{"maintained_at", `ALTER TABLE activity_search_state ADD COLUMN maintained_at INTEGER NOT NULL DEFAULT 0`},
	} {
		exists, err := h.tableHasColumn(ctx, "activity_search_state", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := h.db.ExecContext(ctx, column.ddl); err != nil {
			return fmt.Errorf("state table column %s: %w", column.name, err)
		}
	}
	if _, err := h.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO activity_search_state (id) VALUES (1)`); err != nil {
		return fmt.Errorf("state row: %w", err)
	}

	definition, err := h.schemaSQL(ctx, "activity_fts")
	if err != nil {
		return err
	}
	var version int
	if err := h.db.QueryRowContext(ctx,
		`SELECT version FROM activity_search_state WHERE id = 1`).Scan(&version); err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	// Rebuild when the table belongs to another design, when the state row is
	// from another design, or when the table is missing while the state claims
	// a finished index (a hand-edited database): a partial index must never be
	// mistaken for a complete one.
	rebuild := definition == "" ||
		version != searchSchemaVersion ||
		!isSearchTable(definition)
	if rebuild {
		for _, statement := range searchTriggers {
			if _, err := h.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("drop old trigger: %w", err)
			}
		}
		if _, err := h.db.ExecContext(ctx, `DROP TABLE IF EXISTS activity_fts`); err != nil {
			return fmt.Errorf("drop old index: %w", err)
		}
	}
	if _, err := h.db.ExecContext(ctx, searchTableDDL); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	// The merge policy is persisted in the index's config table, so applying it
	// again on every start is a no-op.
	for _, option := range []struct {
		name  string
		value int
	}{{"automerge", searchAutomerge}, {"crisismerge", searchCrisisMerge}} {
		if _, err := h.db.ExecContext(ctx,
			`INSERT INTO activity_fts (activity_fts, rank) VALUES (?, ?)`,
			option.name, option.value); err != nil {
			return fmt.Errorf("index option %s: %w", option.name, err)
		}
	}
	switch {
	case rebuild:
		if _, err := h.db.ExecContext(ctx, `
			UPDATE activity_search_state
			SET indexed_through = 0, complete = 0, version = ?, maintained_at = 0
			WHERE id = 1`, searchSchemaVersion); err != nil {
			return fmt.Errorf("reset state: %w", err)
		}
	case version != searchSchemaVersion:
		if _, err := h.db.ExecContext(ctx,
			`UPDATE activity_search_state SET version = ? WHERE id = 1`,
			searchSchemaVersion); err != nil {
			return fmt.Errorf("save version: %w", err)
		}
	}
	return nil
}

// isSearchTable reports whether an existing activity_fts definition is the
// content-bearing trigram table this design maintains. An external-content
// definition (content='activity') or another tokenizer is rebuilt.
func isSearchTable(definition string) bool {
	lower := strings.ToLower(definition)
	return strings.Contains(lower, "tokenize='trigram'") && !strings.Contains(lower, "content=")
}

func (h *History) tableHasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("read %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read %s columns: %w", table, err)
	}
	return false, nil
}

func (h *History) schemaSQL(ctx context.Context, name string) (string, error) {
	var definition string
	err := h.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read schema of %s: %w", name, err)
	}
	return definition, nil
}

// warmSearchIndex rebuilds the index for an upgraded database in the
// background. Startup must not wait for a 20,000-row tokenization, and a
// search answers correctly meanwhile: see indexedIDs. A failure leaves the
// cursor where it stopped and logs one warning; searches then stay complete
// through the un-indexed tail, which is slower but correct, until the next
// restart.
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

// compactSearchIndex runs the bounded index maintenance: a merge of at most
// searchMergePages pages, a full compaction at most every
// searchOptimizeInterval, and a bounded incremental-vacuum pass. Errors are
// logged, never returned: maintenance must not fail retention.
func (h *History) compactSearchIndex(ctx context.Context, force bool) {
	if h == nil {
		return
	}
	if err := h.ensureSearchIndex(ctx); err != nil {
		return
	}
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO activity_fts (activity_fts, rank) VALUES ('merge', ?)`,
		searchMergePages); err != nil {
		if !isClosedDatabase(err) {
			slog.Default().Warn("activity search index merge", "error", err.Error())
		}
		return
	}
	var maintained int64
	if err := h.db.QueryRowContext(ctx,
		`SELECT maintained_at FROM activity_search_state WHERE id = 1`).Scan(&maintained); err != nil {
		return
	}
	if force || time.Since(time.UnixMilli(maintained)) >= searchOptimizeInterval {
		if _, err := h.db.ExecContext(ctx,
			`INSERT INTO activity_fts (activity_fts) VALUES ('optimize')`); err != nil {
			if !isClosedDatabase(err) {
				slog.Default().Warn("activity search index optimize", "error", err.Error())
			}
			return
		}
		if _, err := h.db.ExecContext(ctx,
			`UPDATE activity_search_state SET maintained_at = ? WHERE id = 1`,
			time.Now().UnixMilli()); err != nil {
			return
		}
	}
	// Best effort: returns free pages to the filesystem only on a database
	// opened with auto_vacuum=INCREMENTAL, and is otherwise a no-op.
	if _, err := h.db.ExecContext(ctx,
		fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, searchVacuumPages)); err != nil &&
		!isClosedDatabase(err) {
		slog.Default().Warn("activity search index incremental vacuum", "error", err.Error())
	}
}

// isClosedDatabase reports the shutdown race between a maintenance step and
// Close, which is not worth a log line.
func isClosedDatabase(err error) bool {
	return errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "database is closed")
}

// indexEntry writes one row's haystack into the index inside the caller's
// transaction. A rowid that is already indexed is retracted first, so the
// write is a replace rather than a duplicate.
func indexEntry(ctx context.Context, tx *sql.Tx, id int64, search string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM activity_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("activity search index: retract row: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_fts (rowid, search) VALUES (?, ?)`, id, search); err != nil {
		return fmt.Errorf("activity search index: index row: %w", err)
	}
	return nil
}

// searchChunkIDs reads one bounded candidate set from the index.
func (h *History) searchChunkIDs(ctx context.Context, needle string, before int64, limit int) ([]int64, error) {
	rows, err := h.db.QueryContext(ctx, searchChunkSQL, ftsPhrase(needle), before, limit)
	if err != nil {
		return nil, fmt.Errorf("activity history: search index: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("activity history: search index: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("activity history: search index: %w", err)
	}
	return ids, nil
}

// position is one candidate row's position in the page order.
type position struct {
	id int64
	at int64
}

// positionsByID reads the page position of the candidates that still pass the
// filter and cursor. It reads two small columns, never the sixteen a page
// shows, so a chunk of candidates costs a bounded lookup.
func (h *History) positionsByID(ctx context.Context, ids []int64, conditions []string, args []any) ([]position, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	statement := `SELECT id, at FROM activity WHERE id IN (` + placeholders + `)`
	if len(conditions) > 0 {
		statement += " AND " + strings.Join(conditions, " AND ")
	}
	queryArgs := make([]any, 0, len(ids)+len(args))
	for _, id := range ids {
		queryArgs = append(queryArgs, id)
	}
	queryArgs = append(queryArgs, args...)
	rows, err := h.db.QueryContext(ctx, statement, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("activity history: query: %w", err)
	}
	defer rows.Close()
	var positions []position
	for rows.Next() {
		var p position
		if err := rows.Scan(&p.id, &p.at); err != nil {
			return nil, fmt.Errorf("activity history: scan: %w", err)
		}
		positions = append(positions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("activity history: read: %w", err)
	}
	return positions, nil
}

// unindexedPositions answers the not-yet-rebuilt tail while the index is
// incomplete: every row above the cursor is missing from the index, so it is
// matched with the old substring rule over that bounded remainder. Once the
// rebuild finishes this branch is gone.
func (h *History) unindexedPositions(ctx context.Context, through int64, conditions []string, args []any, needle string, limit int) ([]position, error) {
	statement := `SELECT id, at FROM activity WHERE id > ?`
	if len(conditions) > 0 {
		statement += " AND " + strings.Join(conditions, " AND ")
	}
	statement += ` AND instr(search, ?) > 0 ORDER BY at DESC, id DESC LIMIT ?`
	queryArgs := make([]any, 0, len(args)+3)
	queryArgs = append(queryArgs, through)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, needle, limit)
	rows, err := h.db.QueryContext(ctx, statement, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("activity history: positions query (%d args): %w\n%s", len(queryArgs), err, statement)
	}
	defer rows.Close()
	var positions []position
	for rows.Next() {
		var p position
		if err := rows.Scan(&p.id, &p.at); err != nil {
			return nil, fmt.Errorf("activity history: scan: %w", err)
		}
		positions = append(positions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("activity history: read: %w", err)
	}
	return positions, nil
}

// probeWindowSQL is the dense-needle probe: the same substring test the scan
// used, bounded to the newest rows by a time band read from the at index. The
// band is the position of the (limit + slack + 1)th newest row, so the scan
// below it can visit at most that many rows plus a tie group, and a needle
// that matches most of the window fills a page without touching the index.
// Ties share one `at`, so the band never splits a tie group.
func probeWindowSQL(conditions []string) string {
	outer, inner := " WHERE", ""
	if len(conditions) > 0 {
		joined := strings.Join(conditions, " AND ")
		outer = " WHERE " + joined + " AND"
		inner = " WHERE " + joined
	}
	return `SELECT ` + entryColumns + ` FROM activity` + outer + ` at >= (
			SELECT at FROM activity` + inner + ` ORDER BY at DESC, id DESC LIMIT 1 OFFSET ?)
		AND instr(search, ?) > 0
		ORDER BY at DESC, id DESC LIMIT ?`
}

// newestWindowPage is the fast path for a needle that matches most of the
// window. It reads the newest rows once, all columns included, and tests the
// same substring rule the scan used; the read is bounded by the materialized
// LIMIT, so a needle that matches nothing still costs a fixed number of rows
// instead of the retained window. When the window fills a page the answer is
// exact and no index read is needed: every row outside the window is older
// than every row inside it.
func (h *History) newestWindowPage(ctx context.Context, conditions []string, args []any, needle string, limit int) (Page, bool, error) {
	queryArgs := make([]any, 0, 2*len(args)+3)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, limit+searchWindowSlack, needle, limit+1)
	rows, err := h.db.QueryContext(ctx, probeWindowSQL(conditions), queryArgs...)
	if err != nil {
		return Page{}, false, fmt.Errorf("activity history: query: %w", err)
	}
	defer rows.Close()
	var entries []Entry
	var cursors []string
	for rows.Next() {
		scanned, err := scanEntry(rows)
		if err != nil {
			return Page{}, false, err
		}
		scanned.entry.Cursor = scanned.cursor
		entries = append(entries, scanned.entry)
		cursors = append(cursors, scanned.cursor)
	}
	if err := rows.Err(); err != nil {
		return Page{}, false, fmt.Errorf("activity history: read: %w", err)
	}
	if len(entries) <= limit {
		// The window did not hold a full page; the index answers it instead.
		return Page{}, false, nil
	}
	page := Page{Entries: entries[:limit], Next: cursors[limit-1]}
	return page, true, nil
}

// indexedIDs answers a search from the index in bounded chunks. Candidates are
// read newest-id first, filtered and ordered by the page's own (at, id)
// position, and the loop stops as soon as a full page is in hand. While a
// rebuild is incomplete the un-indexed tail is added through the old substring
// rule, so a search is complete at every moment rather than silently partial.
//
// Two orderings meet here: the chunk walk is by rowid, the page is by at. They
// agree because an Activity row's id is assigned at insert and at is the time
// of that request, so a later id is never materially older; the same
// correlation is what makes the keyset cursor on (at, id) meaningful. The walk
// stops on the first chunk that fills a page, so an artificially backdated row
// (a test fixture, not a writer) could be surfaced one page later than exact
// order.
func (h *History) indexedIDs(ctx context.Context, conditions []string, args []any, needle string, limit int) ([]int64, error) {
	state, err := h.searchIndexState(ctx)
	if err != nil {
		return nil, err
	}
	var found []position
	boundary := int64(math.MaxInt64)
	for {
		ids, err := h.searchChunkIDs(ctx, needle, boundary, searchPageChunk)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		boundary = ids[len(ids)-1]
		kept, err := h.positionsByID(ctx, ids, conditions, args)
		if err != nil {
			return nil, err
		}
		found = append(found, kept...)
		if len(found) > limit || len(ids) < searchPageChunk {
			break
		}
	}
	if !state.Complete {
		tail, err := h.unindexedPositions(ctx, state.IndexedThrough, conditions, args, needle, limit+1)
		if err != nil {
			return nil, err
		}
		found = append(found, tail...)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].at != found[j].at {
			return found[i].at > found[j].at
		}
		return found[i].id > found[j].id
	})
	seen := make(map[int64]bool, len(found))
	ids := make([]int64, 0, len(found))
	for _, p := range found {
		if seen[p.id] {
			continue
		}
		seen[p.id] = true
		ids = append(ids, p.id)
		if len(ids) == limit+1 {
			break
		}
	}
	return ids, nil
}

// searchPage answers an indexable text query: the dense-needle probe first,
// then the bounded candidate walk.
func (h *History) searchPage(ctx context.Context, conditions []string, args []any, needle string, limit int) (Page, error) {
	if page, filled, err := h.newestWindowPage(ctx, conditions, args, needle, limit); err != nil {
		return Page{}, err
	} else if filled {
		return page, nil
	}
	ids, err := h.indexedIDs(ctx, conditions, args, needle, limit)
	if err != nil {
		return Page{}, err
	}
	if len(ids) == 0 {
		return Page{}, nil
	}
	return h.pageByID(ctx, ids, limit)
}

// searchIndexState reads the persisted rebuild cursor. It is one primary-key
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

// windowedSearchSQL is the bounded fallback: a materialized window of the
// newest rows, filtered by the same substring rule the index replaces. The
// materialization applies the LIMIT before the haystack test, so the window
// bounds the rows read, not the rows returned.
func windowedSearchSQL(conditions []string) string {
	statement := `WITH window AS MATERIALIZED (
		SELECT id, at, search FROM activity`
	if len(conditions) > 0 {
		statement += " WHERE " + strings.Join(conditions, " AND ")
	}
	return statement + `
		ORDER BY at DESC, id DESC LIMIT ?)
		SELECT id FROM window WHERE instr(search, ?) > 0
		ORDER BY at DESC, id DESC LIMIT ?`
}

// pageByWindow answers a search from the newest ShortSearchWindow rows only.
func (h *History) pageByWindow(ctx context.Context, conditions []string, args []any, needle string, limit int) (Page, error) {
	windowArgs := make([]any, 0, len(args)+3)
	windowArgs = append(windowArgs, args...)
	windowArgs = append(windowArgs, ShortSearchWindow, needle, limit+1)

	rows, err := h.db.QueryContext(ctx, windowedSearchSQL(conditions), windowArgs...)
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
	if len(ids) == 0 {
		return Page{}, nil
	}
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
