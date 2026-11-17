// Durable Activity history (#96): the ring in activity.go is the fast first
// page, and this file is the source of truth for anything older. It is a
// display-only table in the assistant's one SQLite store (ADR-0003): the
// Dashboard is its only reader, and a Local-only Tool result is still shown
// only on the authenticated page. Nothing here is logged, sent to a model, or
// read back from container logs.
package activity

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFullWindow is how long an entry keeps its whole transcript and
	// reply. Older rows are trimmed to a summary so the file stays bounded.
	DefaultFullWindow = 7 * 24 * time.Hour
	// DefaultMaxAge deletes a row entirely after this long.
	DefaultMaxAge = 30 * 24 * time.Hour
	// DefaultMaxRows is the hard row bound, independent of age.
	DefaultMaxRows = 20000

	// summaryRunes bounds a trimmed text field; the row still answers "what
	// kind of request was this" without keeping a whole conversation forever.
	summaryRunes = 160
	// searchRunes bounds the haystack a search scans for one row.
	searchRunes = 4000
	// maxQueryRunes bounds the free-text query so one request cannot make the
	// database scan an unbounded pattern.
	maxQueryRunes = 120
	// pruneBatch bounds one retention pass, so an hourly tick never holds the
	// single writer for long.
	pruneBatch = 500

	// DefaultPageLimit is one page of durable history; the handler clamps a
	// client value to maxPageLimit.
	DefaultPageLimit = 25
	maxPageLimit     = 100
)

// HistoryOptions configures retention. Zero values take the defaults, which
// are also the policy documented on #96.
type HistoryOptions struct {
	FullWindow time.Duration
	MaxAge     time.Duration
	MaxRows    int
	// Now overrides the clock for deterministic tests.
	Now func() time.Time
	// SkipSearchWarmup keeps NewHistory from starting the background rebuild of
	// the trigram search index. A test that seeds a large fixture sets it so the
	// fixture is stable, and drives WarmSearchIndex itself. A search still
	// creates the index and stays correct meanwhile (searchindex.go).
	SkipSearchWarmup bool
}

// History is the durable Activity table. It is safe for concurrent use; the
// database handle is the assistant's single writer connection.
type History struct {
	db         *sql.DB
	fullWindow time.Duration
	maxAge     time.Duration
	maxRows    int
	now        func() time.Time
	// pruneMu keeps two prune calls (startup and the hourly tick) from
	// interleaving their read-then-update batches.
	pruneMu sync.Mutex
	// schemaMu and schemaReady apply the search index schema once; schemaErr
	// remembers a build that cannot create it, so a search degrades to the
	// bounded window instead of retrying DDL on every request.
	schemaMu    sync.Mutex
	schemaReady bool
	schemaErr   error
	// warmMu keeps two index rebuilds (the background loop and a caller that
	// asks for one) from indexing the same batch twice.
	warmMu sync.Mutex
}

// NewHistory wraps the assistant's database. The table is created by store
// migration 11; a missing table is a wiring error, reported by the first Add
// rather than by a schema write here.
func NewHistory(db *sql.DB, opts HistoryOptions) *History {
	h := &History{
		db:         db,
		fullWindow: opts.FullWindow,
		maxAge:     opts.MaxAge,
		maxRows:    opts.MaxRows,
		now:        opts.Now,
	}
	if h.fullWindow <= 0 {
		h.fullWindow = DefaultFullWindow
	}
	if h.maxAge <= 0 {
		h.maxAge = DefaultMaxAge
	}
	if h.maxRows <= 0 {
		h.maxRows = DefaultMaxRows
	}
	if h.now == nil {
		h.now = time.Now
	}
	if !opts.SkipSearchWarmup {
		// An upgraded database may have a whole retained window to tokenize.
		// That must not delay startup, so it runs in bounded batches here; a
		// search is correct while it runs (searchindex.go).
		go h.warmSearchIndex()
	}
	return h
}

// Add writes one completed request. It is called from the latency observer on
// the request's own goroutine, so it is one INSERT and no scan; failure is
// logged and never fails the request that produced the entry.
func (h *History) Add(entry Entry) error {
	if h == nil {
		return nil
	}
	at := entry.At
	if at.IsZero() {
		at = h.now()
	}
	stages, err := json.Marshal(nonNil(entry.Stages))
	if err != nil {
		return fmt.Errorf("activity history: encode stages: %w", err)
	}
	checkpoints, err := json.Marshal(nonNil(entry.Checkpoints))
	if err != nil {
		return fmt.Errorf("activity history: encode checkpoints: %w", err)
	}
	calls, err := json.Marshal(nonNil(entry.Calls))
	if err != nil {
		return fmt.Errorf("activity history: encode calls: %w", err)
	}
	var response any
	if entry.ResponseMS != nil {
		response = *entry.ResponseMS
	}
	const insertActivity = `
		INSERT INTO activity (request_id, at, source, path, tool, outcome, error,
		transcript, reply, local_only, response_ms, stages, checkpoints, calls, search,
		prompt_tokens, completion_tokens, total_tokens, cached_tokens, agent_steps, conversation)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	haystack := searchText(entry)
	insertArgs := []any{
		entry.ID, at.UnixMilli(), entry.Source, entry.Path, entry.Tool, entry.Outcome,
		entry.Error, entry.Transcript, entry.Reply, entry.LocalOnly, response,
		string(stages), string(checkpoints), string(calls), haystack,
		entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens, entry.CachedTokens, entry.Steps,
		entry.Conversation,
	}
	ctx := context.Background()
	if err := h.ensureSearchIndex(ctx); err != nil {
		// A build without FTS5: keep writing history, mark the index incomplete
		// so a later start that can create it rebuilds the gap, and let searches
		// use the bounded window in the meantime.
		h.markSearchIndexIncomplete(ctx)
		return h.insertActivityOnly(ctx, insertActivity, insertArgs)
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("activity history: insert: %w", err)
	}
	result, err := tx.ExecContext(ctx, insertActivity, insertArgs...)
	if err != nil {
		_ = tx.Rollback()
		slog.Default().Warn("activity history write", "error", err.Error())
		return fmt.Errorf("activity history: insert: %w", err)
	}
	id, err := result.LastInsertId()
	if err == nil {
		err = indexEntry(ctx, tx, id, haystack)
	}
	if err != nil {
		// The row is the source of truth and the index is derived: commit the
		// row without it and let the rebuild fill the gap rather than losing the
		// request from history.
		_ = tx.Rollback()
		slog.Default().Warn("activity history index write", "error", err.Error())
		h.markSearchIndexIncomplete(ctx)
		return h.insertActivityOnly(ctx, insertActivity, insertArgs)
	}
	if err := tx.Commit(); err != nil {
		slog.Default().Warn("activity history write", "error", err.Error())
		return fmt.Errorf("activity history: insert: %w", err)
	}
	return nil
}

// insertActivityOnly writes the row without an index entry, for the degraded
// path where the index is missing or its write failed.
func (h *History) insertActivityOnly(ctx context.Context, statement string, args []any) error {
	if _, err := h.db.ExecContext(ctx, statement, args...); err != nil {
		slog.Default().Warn("activity history write", "error", err.Error())
		return fmt.Errorf("activity history: insert: %w", err)
	}
	return nil
}

// markSearchIndexIncomplete drops the completion latch after a write that the
// index did not take, so the next rebuild re-indexes exactly the gap above the
// persisted cursor instead of trusting an index that is now behind.
func (h *History) markSearchIndexIncomplete(ctx context.Context) {
	if _, err := h.db.ExecContext(ctx,
		`UPDATE activity_search_state SET complete = 0 WHERE id = 1 AND complete = 1`); err != nil &&
		!isClosedDatabase(err) {
		slog.Default().Warn("activity search index state", "error", err.Error())
	}
}

// Query is one bounded history read: a filter, an optional free-text search,
// and a keyset cursor. Limit is clamped to 1..100 and defaults to 25.
type Query struct {
	Filter string
	Text   string
	Limit  int
	Before string
}

// Page is one newest-first page. Next is the cursor for the page below this
// one, empty when the history is exhausted.
type Page struct {
	Entries []Entry
	Next    string
	// SearchWindow is non-empty when a text query could only read part of the
	// retained history. The page shows that limit instead of implying that the
	// whole store was searched (searchindex.go).
	SearchWindow SearchWindow
}

// ErrBadCursor means the caller sent a cursor this version did not mint.
var ErrBadCursor = fmt.Errorf("activity history: invalid cursor")

// ErrBadFilter means the caller sent a filter outside the Activity page's set.
var ErrBadFilter = fmt.Errorf("activity history: unknown filter")

// QueryPage reads one page, newest first. The cursor is keyset-based, so an
// entry written while the caller pages cannot shift or duplicate a row.
func (h *History) QueryPage(ctx context.Context, q Query) (Page, error) {
	if h == nil {
		return Page{}, nil
	}
	filter := strings.TrimSpace(q.Filter)
	if filter == "" {
		filter = "all"
	}
	var conditions []string
	var args []any
	switch filter {
	case "all":
	case "voice":
		conditions = append(conditions, "source = ?")
		args = append(args, "voice")
	case "typed":
		conditions = append(conditions, "source IN (?, ?)")
		args = append(args, "dashboard", "debug")
	case "mic":
		conditions = append(conditions, "source = ?")
		args = append(args, "dashboard_mic")
	case "problems":
		conditions = append(conditions, "(outcome IN (?, ?) OR error <> '')")
		args = append(args, "failed", "cancelled")
	default:
		return Page{}, ErrBadFilter
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if q.Before != "" {
		at, id, err := decodeCursor(q.Before)
		if err != nil {
			return Page{}, err
		}
		conditions = append(conditions, "(at < ? OR (at = ? AND id < ?))")
		args = append(args, at, at, id)
	}
	if needle := normalizeQuery(q.Text); needle != "" {
		// The trigram index answers a three-character or longer query; a shorter
		// one, or a build without the index, reads only the newest
		// ShortSearchWindow rows. Neither walks the retained window, and both
		// say so on the page rather than returning a silent partial answer.
		switch {
		case len([]rune(needle)) < minTrigramRunes:
			page, err := h.pageByWindow(ctx, conditions, args, needle, limit)
			page.SearchWindow = SearchWindowShort
			return page, err
		case h.ensureSearchIndex(ctx) != nil:
			page, err := h.pageByWindow(ctx, conditions, args, needle, limit)
			page.SearchWindow = SearchWindowIndexUnavailable
			return page, err
		default:
			return h.searchPage(ctx, conditions, args, needle, limit)
		}
	}

	statement := `SELECT ` + entryColumns + ` FROM activity`
	if len(conditions) > 0 {
		statement += " WHERE " + strings.Join(conditions, " AND ")
	}
	statement += " ORDER BY at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := h.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return Page{}, fmt.Errorf("activity history: query: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	var cursors []string
	for rows.Next() {
		scanned, err := scanEntry(rows)
		if err != nil {
			return Page{}, err
		}
		scanned.entry.Cursor = scanned.cursor
		entries = append(entries, scanned.entry)
		cursors = append(cursors, scanned.cursor)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("activity history: read: %w", err)
	}

	page := Page{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.Next = cursors[limit-1]
	}
	return page, nil
}

// Count returns the number of retained rows; it is the bounded-growth check.
func (h *History) Count(ctx context.Context) (int, error) {
	if h == nil {
		return 0, nil
	}
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity`).Scan(&n); err != nil {
		return 0, fmt.Errorf("activity history: count: %w", err)
	}
	return n, nil
}

// PruneResult reports one retention pass.
type PruneResult struct {
	Deleted   int64
	Trimmed   int64
	Remaining int
}

// Prune enforces the retention policy: delete rows past MaxAge, keep at most
// MaxRows, and trim long text on rows older than FullWindow to a summary. It
// is idempotent and bounded per call. Run it at startup and then hourly.
func (h *History) Prune(ctx context.Context) (PruneResult, error) {
	if h == nil {
		return PruneResult{}, nil
	}
	h.pruneMu.Lock()
	defer h.pruneMu.Unlock()

	// Retention is the one path that can meet a half-built index: on the first
	// start after an upgrade the rebuild may not have reached the rows this
	// pass deletes. The index is maintained explicitly here (no triggers), so a
	// row the rebuild has not indexed yet is simply absent from the retraction
	// and nothing raises SQLITE_CORRUPT.
	indexed := h.ensureSearchIndex(ctx) == nil
	var result PruneResult
	now := h.now()
	cutoff := now.Add(-h.maxAge).UnixMilli()
	deleted, err := h.deleteRows(ctx, indexed, `at < ?`, cutoff)
	if err != nil {
		return result, fmt.Errorf("activity history: age prune: %w", err)
	}
	result.Deleted = deleted

	// Cap the row count independently of age: a busy week still cannot grow
	// the file past MaxRows entries.
	capped, err := h.deleteRows(ctx, indexed,
		`id NOT IN (SELECT id FROM activity ORDER BY at DESC, id DESC LIMIT ?)`, h.maxRows)
	if err != nil {
		return result, fmt.Errorf("activity history: row cap: %w", err)
	}
	result.Deleted += capped

	fullCutoff := now.Add(-h.fullWindow).UnixMilli()
	// Tool, path and call names stay on the trimmed row, so the read keeps them
	// for the search column below.
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, transcript, reply, error, tool, path, calls
		FROM activity WHERE summary = 0 AND at < ? ORDER BY at ASC LIMIT ?`,
		fullCutoff, pruneBatch)
	if err != nil {
		return result, fmt.Errorf("activity history: trim read: %w", err)
	}
	type trimmable struct {
		id                                            int64
		transcript, reply, failure, tool, path, calls string
	}
	var batch []trimmable
	for rows.Next() {
		var row trimmable
		if err := rows.Scan(&row.id, &row.transcript, &row.reply, &row.failure, &row.tool, &row.path, &row.calls); err != nil {
			rows.Close()
			return result, fmt.Errorf("activity history: trim scan: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("activity history: trim read: %w", err)
	}
	rows.Close()

	if len(batch) > 0 {
		tx, err := h.db.BeginTx(ctx, nil)
		if err != nil {
			return result, fmt.Errorf("activity history: trim begin: %w", err)
		}
		for _, row := range batch {
			transcript := truncate(row.transcript, summaryRunes)
			reply := truncate(row.reply, summaryRunes)
			failure := truncate(row.failure, summaryRunes)
			calls := summarizeCalls(row.calls)
			// The stored haystack follows what the trimmed row still shows: the
			// short text plus the Tool, path and call names it keeps.
			entry := Entry{Transcript: transcript, Reply: reply, Error: failure, Tool: row.tool, Path: row.path}
			_ = json.Unmarshal([]byte(calls), &entry.Calls)
			haystack := searchText(entry)
			if _, err := tx.ExecContext(ctx, `
				UPDATE activity SET transcript = ?, reply = ?, error = ?, calls = ?,
					search = ?, summary = 1 WHERE id = ?`,
				transcript, reply, failure, calls, haystack, row.id); err != nil {
				tx.Rollback()
				return result, fmt.Errorf("activity history: trim update: %w", err)
			}
			if indexed {
				// The index entry follows the shortened haystack: dropped text
				// stops matching, kept text keeps matching.
				if err := indexEntry(ctx, tx, row.id, haystack); err != nil {
					tx.Rollback()
					return result, fmt.Errorf("activity history: trim index: %w", err)
				}
			}
			result.Trimmed++
		}
		if err := tx.Commit(); err != nil {
			return result, fmt.Errorf("activity history: trim commit: %w", err)
		}
	}

	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity`).Scan(&result.Remaining); err != nil {
		return result, fmt.Errorf("activity history: count after prune: %w", err)
	}
	if indexed && (result.Deleted > 0 || result.Trimmed > 0) {
		// A pass that removed postings leaves dead space in the index and free
		// pages in the file. One bounded merge, plus at most one full compaction
		// every searchOptimizeInterval, keeps that space reusable instead of
		// ratcheting the file to the peak haystack. It runs here, after the
		// retention writes, and never on the request goroutine.
		h.compactSearchIndex(ctx, false)
	}
	return result, nil
}

// deleteRows removes the activity rows matching one retention predicate and
// their index entries in the same transaction, so a delete cannot leave an
// index entry behind or fail because the index never held the row.
func (h *History) deleteRows(ctx context.Context, indexed bool, predicate string, args ...any) (int64, error) {
	if !indexed {
		deleted, err := h.db.ExecContext(ctx, `DELETE FROM activity WHERE `+predicate, args...)
		if err != nil {
			return 0, err
		}
		affected, _ := deleted.RowsAffected()
		return affected, nil
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM activity_fts WHERE rowid IN (SELECT id FROM activity WHERE `+predicate+`)`,
		args...); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM activity WHERE `+predicate, args...)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	affected, _ := deleted.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return affected, nil
}

// Matches applies the Activity page's filter and free-text search to an Entry
// that is only in the ring. The durable query uses the same rules in SQL, so a
// page reads the same whether a row came from memory or the database.
func (e Entry) Matches(filter, text string) bool {
	switch filter {
	case "", "all":
	case "voice":
		if e.Source != "voice" {
			return false
		}
	case "typed":
		if e.Source != "dashboard" && e.Source != "debug" {
			return false
		}
	case "mic":
		if e.Source != "dashboard_mic" {
			return false
		}
	case "problems":
		if e.Outcome != "failed" && e.Outcome != "cancelled" && e.Error == "" {
			return false
		}
	default:
		return false
	}
	needle := normalizeQuery(text)
	if needle == "" {
		return true
	}
	return strings.Contains(searchText(e), needle)
}

// searchText is the one haystack definition: everything the row shows, plus
// the humanized path so "fast path" finds fast_path. It is lowercased once at
// write time; a read only compares.
func searchText(e Entry) string {
	parts := []string{e.Transcript, e.Reply, e.Tool, e.Path, e.Error}
	if human := strings.ReplaceAll(e.Path, "_", " "); human != e.Path {
		parts = append(parts, human)
	}
	for _, call := range e.Calls {
		parts = append(parts, call.Name)
	}
	text := strings.ToLower(strings.Join(parts, " "))
	return truncate(collapseSpace(text), searchRunes)
}

// normalizeQuery trims, lowercases and collapses one client query, capped so a
// pasted document cannot become an unbounded pattern. It collapses whitespace
// for the same reason the haystack does, and cuts without an ellipsis because
// the cap is a rune boundary, not a display. A NUL byte is dropped: it is not
// text, and it would truncate an FTS5 query string into a syntax error rather
// than a plain "no matches".
func normalizeQuery(text string) string {
	text = strings.ReplaceAll(text, "\x00", "")
	needle := collapseSpace(strings.ToLower(strings.TrimSpace(text)))
	if runes := []rune(needle); len(runes) > maxQueryRunes {
		needle = string(runes[:maxQueryRunes])
	}
	return needle
}

// collapseSpace folds runs of whitespace so a multi-word query matches across
// a newline in the transcript or reply.
func collapseSpace(text string) string { return strings.Join(strings.Fields(text), " ") }

// summarizeCalls keeps each Tool name and Local-only marker but drops the
// result text, which is the sensitive, long part of an old entry.
func summarizeCalls(raw string) string {
	var calls []Call
	if err := json.Unmarshal([]byte(raw), &calls); err != nil || len(calls) == 0 {
		return "[]"
	}
	for i := range calls {
		calls[i].Result = ""
	}
	encoded, err := json.Marshal(calls)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// entryColumns is the one SELECT list for a full history row; scanEntry reads
// exactly these columns in this order.
const entryColumns = `id, request_id, at, source, path, tool, outcome, error,
	transcript, reply, local_only, response_ms, stages, checkpoints, calls, summary,
	prompt_tokens, completion_tokens, total_tokens, cached_tokens, agent_steps, conversation`

// scanResult is one scanned row: the integer id keys the windowed page's
// ordered id list, the cursor is what the page hands the client, and the entry
// is what it shows.
type scanResult struct {
	id     int64
	cursor string
	entry  Entry
}

// scanEntry reads one row into an Entry and its cursor. Column order is
// entryColumns.
func scanEntry(rows *sql.Rows) (scanResult, error) {
	var (
		entry                      Entry
		id, at                     int64
		stages, checkpoints, calls string
		requestID                  string
		response                   sql.NullInt64
		prompt, completion         sql.NullInt64
		total, cached              sql.NullInt64
		agentSteps                 sql.NullInt64
		localOnly, summary         bool
	)
	if err := rows.Scan(&id, &requestID, &at, &entry.Source, &entry.Path, &entry.Tool,
		&entry.Outcome, &entry.Error, &entry.Transcript, &entry.Reply, &localOnly,
		&response, &stages, &checkpoints, &calls, &summary,
		&prompt, &completion, &total, &cached, &agentSteps, &entry.Conversation); err != nil {
		return scanResult{}, fmt.Errorf("activity history: scan: %w", err)
	}
	entry.ID = requestID
	entry.At = time.UnixMilli(at)
	entry.LocalOnly = localOnly
	entry.Summary = summary
	// A NULL column is a row written before migration 14. Zero reads as
	// unknown, never as a free turn.
	entry.PromptTokens = prompt.Int64
	entry.CompletionTokens = completion.Int64
	entry.TotalTokens = total.Int64
	entry.CachedTokens = cached.Int64
	if agentSteps.Valid {
		entry.Steps = int(agentSteps.Int64)
	}
	if response.Valid {
		ms := response.Int64
		entry.ResponseMS = &ms
	}
	if err := json.Unmarshal([]byte(stages), &entry.Stages); err != nil {
		return scanResult{}, fmt.Errorf("activity history: decode stages: %w", err)
	}
	if err := json.Unmarshal([]byte(checkpoints), &entry.Checkpoints); err != nil {
		return scanResult{}, fmt.Errorf("activity history: decode checkpoints: %w", err)
	}
	if err := json.Unmarshal([]byte(calls), &entry.Calls); err != nil {
		return scanResult{}, fmt.Errorf("activity history: decode calls: %w", err)
	}
	return scanResult{id: id, cursor: encodeCursor(at, id), entry: entry}, nil
}

// encodeCursor and decodeCursor wrap the (at, rowid) pair the keyset page
// needs. The value is opaque to the client and validated on the way back.
func encodeCursor(at, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + ":" + strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (int64, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, ErrBadCursor
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return 0, 0, ErrBadCursor
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, ErrBadCursor
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, ErrBadCursor
	}
	return at, id, nil
}

// nonNil keeps a JSON column the documented "[]" for an empty slice instead of
// JSON null, so a read never has to special-case nil.
func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
