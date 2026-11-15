package activity_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// The bounded-window tests use realistic row counts: the row cap is 20,000
// (activity.DefaultMaxRows), and a no-match search there is exactly the case
// #210 measured at 44-110 ms on x86 and worse on a Pi.
const (
	largeWindowRows = 20000
	smallWindowRows = 2000
	// fillerHaystack is about 1 KB, the size the issue measured with.
	fillerHaystack = 1000
	// searchCeiling is the acceptance bound for one no-match search over the
	// whole retained window. The scan it replaces took 59-88 ms locally and
	// 190 ms with 4 KB haystacks; an indexed answer is well under a
	// millisecond, so this leaves room for a loaded CI machine and still fails
	// a reintroduced scan. raceSlowdown (race_on_test.go) scales it under the
	// race detector, which slows the driver by an order of magnitude.
	searchCeiling = 20 * time.Millisecond * raceSlowdown
)

var fillerWords = strings.Fields("the quick brown fox jumps over a lazy dog " +
	"while reports arrive hourly from the radio station and calendar events pile up in the folder")

// seedWindow fills the activity table directly, before the search index
// exists, so seeding does not pay for tokenization. Row n is "req-<n>" at one
// minute after the previous row, with a haystack of about `haystack` bytes, and
// the caller's marker text is prepended to the marked rows.
//
// The rows go in with one set-based statement: 20,000 individual inserts plus
// their commits is most of this package's test time, and nothing here depends
// on the write path.
func seedWindow(t *testing.T, rows, haystack int, markers map[int]string) (*activity.History, *store.Store) {
	t.Helper()
	st := openStore(t)
	seedRawRows(t, st, rows, haystack)
	for i, marker := range markers {
		if _, err := st.DB().Exec(`
			UPDATE activity SET search = ? || ' ' || search WHERE request_id = ?`,
			marker, fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("mark row %d: %v", i, err)
		}
	}
	return activity.NewHistory(st.DB(), activity.HistoryOptions{SkipSearchWarmup: true}), st
}

// openStore opens an empty assistant database in a temp directory.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// warmSearchIndex drives the rebuild the way the background loop does, so a
// test starts from the steady state instead of racing it.
func warmSearchIndex(t *testing.T, history *activity.History) activity.SearchIndexState {
	t.Helper()
	for i := 0; i < 10000; i++ {
		state, err := history.WarmSearchIndex(context.Background())
		if err != nil {
			t.Fatalf("WarmSearchIndex: %v", err)
		}
		if state.Complete {
			return state
		}
	}
	t.Fatal("search index never finished rebuilding")
	return activity.SearchIndexState{}
}

// bestQueryTime returns the fastest of runs identical searches. A scan of the
// retained window cannot be that fast at these row counts, and the minimum is
// the observation least polluted by a scheduler or collector pause.
func bestQueryTime(t *testing.T, history *activity.History, text string, runs int) (time.Duration, int) {
	t.Helper()
	best := time.Duration(math.MaxInt64)
	matched := 0
	for i := 0; i < runs; i++ {
		started := time.Now()
		page, err := history.QueryPage(context.Background(), activity.Query{Text: text, Limit: 25})
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("QueryPage(%q): %v", text, err)
		}
		matched = len(page.Entries)
		if elapsed < best {
			best = elapsed
		}
	}
	return best, matched
}

// TestSearchIsBoundedOnTheRetainedWindow is the #210 acceptance test: a
// no-match search over a full 20,000-row window is answered from the trigram
// index, a real match still returns the same rows, and the cost does not grow
// with the window.
func TestSearchIsBoundedOnTheRetainedWindow(t *testing.T) {
	// One rare marker near the oldest end, so the match cannot come from a
	// newest-page shortcut.
	history, _ := seedWindow(t, largeWindowRows, fillerHaystack, map[int]string{5: "weather needle"})
	warmSearchIndex(t, history)
	ctx := context.Background()

	t.Run("a no-match search is answered from the index", func(t *testing.T) {
		elapsed, matched := bestQueryTime(t, history, "zzzz-not-present", 20)
		if matched != 0 {
			t.Fatalf("matched %d rows for a needle that is not in the window", matched)
		}
		if elapsed > searchCeiling {
			t.Fatalf("no-match search over %d rows took %v, want under %v",
				largeWindowRows, elapsed, searchCeiling)
		}
		t.Logf("no-match search over %d rows: %v", largeWindowRows, elapsed)
	})

	t.Run("a match still returns the row", func(t *testing.T) {
		page, err := history.QueryPage(ctx, activity.Query{Text: "weather needle", Limit: 25})
		if err != nil {
			t.Fatalf("QueryPage: %v", err)
		}
		if len(page.Entries) != 1 || page.Entries[0].ID != "req-5" {
			t.Fatalf("matches = %d (%v), want only req-5", len(page.Entries), ids(page.Entries))
		}
		// A three-character substring of a word still matches, which the
		// debounced search-as-you-type box relies on.
		page, err = history.QueryPage(ctx, activity.Query{Text: "eath", Limit: 25})
		if err != nil {
			t.Fatalf("QueryPage: %v", err)
		}
		if len(page.Entries) != 1 || page.Entries[0].ID != "req-5" {
			t.Fatalf("substring matches = %v, want only req-5", ids(page.Entries))
		}
	})

	t.Run("cost does not grow with the window", func(t *testing.T) {
		small, _ := seedWindow(t, smallWindowRows, fillerHaystack, nil)
		smallWarm := warmSearchIndex(t, small)
		smallElapsed, _ := bestQueryTime(t, small, "zzzz-not-present", 20)
		largeElapsed, _ := bestQueryTime(t, history, "zzzz-not-present", 20)
		t.Logf("%d rows: %v; %d rows: %v (%+v)", smallWindowRows, smallElapsed,
			largeWindowRows, largeElapsed, smallWarm)
		// A ten-fold window must not cost ten times as much: an index lookup is
		// flat, a scan is linear. The constant absorbs per-query overhead.
		if largeElapsed > 3*smallElapsed+2*time.Millisecond*raceSlowdown {
			t.Fatalf("a 10x larger window took %v vs %v, which is a scan, not an index",
				largeElapsed, smallElapsed)
		}
	})

	// The assistant runs SQLite on one connection, so a search that walks the
	// window also queues an unrelated write behind it. With the search bounded,
	// an insert that arrives during it is not stuck.
	t.Run("a no-match search does not hold the connection", func(t *testing.T) {
		ctx := context.Background()
		const searches = 40
		done := make(chan struct{})
		started := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < searches; i++ {
				if i == 0 {
					close(started)
				}
				if _, err := history.QueryPage(ctx, activity.Query{Text: "zzzz-not-present", Limit: 25}); err != nil {
					t.Errorf("QueryPage: %v", err)
					return
				}
			}
		}()
		<-started

		// Each write must answer while the searches run. The scan this replaces
		// held the connection for 59-88 ms per search, so a write could wait a
		// whole scan; the ceiling is 100x a real insert and still catches that.
		worst := time.Duration(0)
		for i := 0; i < 5; i++ {
			at := time.Now().Add(time.Duration(i) * time.Minute)
			startedAt := time.Now()
			if err := history.Add(entry("write-"+string(rune('a'+i)), at, nil)); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if elapsed := time.Since(startedAt); elapsed > worst {
				worst = elapsed
			}
		}
		<-done
		if worst > searchCeiling {
			t.Fatalf("an unrelated insert waited %v behind a no-match search, want under %v", worst, searchCeiling)
		}
		t.Logf("worst insert latency during %d no-match searches: %v", searches, worst)
	})
}

// TestSearchMatchesTheSameRowsInTheSameOrder keeps the indexed read and the
// in-memory ring rule (Entry.Matches, a plain substring test) in agreement, and
// keeps newest-first order stable across the two.
func TestSearchMatchesTheSameRowsInTheSameOrder(t *testing.T) {
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	entries := []activity.Entry{
		entry("transcript", base, func(e *activity.Entry) {
			e.Source = "dashboard"
			e.Transcript = "remind me about the blue folder"
		}),
		entry("reply", base.Add(time.Minute), func(e *activity.Entry) { e.Reply = "the kettle is on" }),
		entry("tool", base.Add(2*time.Minute), func(e *activity.Entry) {
			e.Tool = "notes.search"
			e.Calls = []activity.Call{{Name: "notes.search", Result: "local only text"}}
		}),
		entry("path", base.Add(3*time.Minute), func(e *activity.Entry) { e.Path = "agent_path" }),
		entry("error", base.Add(4*time.Minute), func(e *activity.Entry) {
			e.Outcome = "failed"
			e.Error = "weather provider timed out"
		}),
		entry("two-words", base.Add(5*time.Minute), func(e *activity.Entry) {
			e.Transcript = "weather forecast for the weekend"
		}),
	}
	for _, e := range entries {
		if err := history.Add(e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// The ring matcher is the reference behavior the SQL used to mirror with
	// instr; the trigram index must not change which rows come back.
	var want []string
	for _, needle := range []string{
		"blue folder",  // transcript, multi-word
		"kettle",       // reply
		"notes.search", // tool name
		"fast path",    // humanized path
		"weather",      // reply, error and transcript
		"eve",          // three-character substring of events and weekend
		"zzzz",         // no match
		"ca",           // two characters: the bounded fallback
		"fo",           // two characters, no match
		`"quote`,       // FTS5 syntax characters are just text
		"NEAR(the weather)",
		"weather*",
		"***",
		"(weather)",
		"weather\x00sea",
		"wéath",
	} {
		// The expected order is newest first, which is the reverse of the seed
		// order above.
		want = want[:0]
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].Matches("all", needle) {
				want = append(want, entries[i].ID)
			}
		}
		page, err := history.QueryPage(ctx, activity.Query{Text: needle, Limit: 20})
		if err != nil {
			t.Fatalf("QueryPage(%q): %v", needle, err)
		}
		if got := strings.Join(ids(page.Entries), ","); got != strings.Join(want, ",") {
			t.Errorf("search %q matched %q, ring matches %q", needle, got, strings.Join(want, ","))
		}
	}
}

// TestPagedSearchDoesNotDuplicateOrSkipRows keeps the keyset cursor honest on
// both search paths: paging an indexed query and paging the bounded window
// fallback must not repeat or drop a row.
func TestPagedSearchDoesNotDuplicateOrSkipRows(t *testing.T) {
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	const total = 40
	for i := 0; i < total; i++ {
		text := "the radio is quiet"
		if i%2 == 0 {
			text = "the kettle is on"
		}
		if err := history.Add(entry(fmt.Sprintf("req-%02d", i), base.Add(time.Duration(i)*time.Minute),
			func(e *activity.Entry) { e.Transcript = text })); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	want := make([]string, 0, total/2)
	for i := total - 2; i >= 0; i -= 2 { // newest first, even ids only
		want = append(want, fmt.Sprintf("req-%02d", i))
	}

	for _, tc := range []struct {
		name string
		text string
	}{
		{"indexed", "kettle is on"},
		{"bounded window", "ke"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			cursor := ""
			for page := 0; page < total; page++ {
				result, err := history.QueryPage(ctx, activity.Query{Text: tc.text, Limit: 3, Before: cursor})
				if err != nil {
					t.Fatalf("QueryPage: %v", err)
				}
				for _, e := range result.Entries {
					seen = append(seen, e.ID)
				}
				cursor = result.Next
				if cursor == "" {
					break
				}
			}
			if len(seen) != len(want) {
				t.Fatalf("paged search for %q returned %d rows, want %d", tc.text, len(seen), len(want))
			}
			if strings.Join(seen, ",") != strings.Join(want, ",") {
				t.Fatalf("paged search for %q order = %v, want %v", tc.text, seen, want)
			}
		})
	}
}

// TestShortQueryReadsOnlyTheNewestWindow covers the documented fallback for
// one and two character queries, which a trigram index cannot answer: it must
// not error, and it must not walk the retained window.
func TestShortQueryReadsOnlyTheNewestWindow(t *testing.T) {
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	const rows = 300
	for i := 0; i < rows; i++ {
		text := fmt.Sprintf("filler line %d", i)
		switch i {
		case 50: // inside the window? no: 249 rows are newer than this one.
			text = "zebra in the old cellar"
		case 250: // 49 rows are newer, so this is inside the newest 200.
			text = "zebra at the gate"
		}
		if err := history.Add(entry(fmt.Sprintf("req-%03d", i), base.Add(time.Duration(i)*time.Minute),
			func(e *activity.Entry) { e.Transcript = text })); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	warmSearchIndex(t, history)

	for _, needle := range []string{"z", "ze"} {
		page, err := history.QueryPage(ctx, activity.Query{Text: needle, Limit: 50})
		if err != nil {
			t.Fatalf("QueryPage(%q): %v", needle, err)
		}
		got := ids(page.Entries)
		if len(got) != 1 || got[0] != "req-250" {
			t.Fatalf("search %q matched %v, want only the row inside the newest window", needle, got)
		}
	}
	// Three characters reach the index, so the older row matches again.
	page, err := history.QueryPage(ctx, activity.Query{Text: "zeb", Limit: 50})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if got := ids(page.Entries); len(got) != 2 || got[0] != "req-250" || got[1] != "req-050" {
		t.Fatalf("three-character search matched %v, want both zebra rows newest first", got)
	}
}

// TestSearchIndexFollowsTrimAndDelete covers the writer half: the triggers must
// carry a trimmed row's kept text into the index, drop the text retention
// removed, and index a new row immediately.
func TestSearchIndexFollowsTrimAndDelete(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	history, st := openHistory(t, activity.HistoryOptions{
		FullWindow: 24 * time.Hour,
		MaxAge:     7 * 24 * time.Hour,
		MaxRows:    100,
		Now:        func() time.Time { return now },
	})
	kept := "kept-marker " + strings.Repeat("a long sentence ", 40)
	dropped := "tail-marker "
	if err := history.Add(entry("trimmed", now.Add(-2*24*time.Hour), func(e *activity.Entry) {
		e.Transcript = kept + dropped
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := history.Add(entry("expired", now.Add(-8*24*time.Hour), func(e *activity.Entry) {
		e.Transcript = "expired-marker transcript"
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	warmSearchIndex(t, history)

	if got := searchIDs(t, history, "tail-marker"); len(got) != 1 {
		t.Fatalf("before the trim, tail-marker matched %v, want the full row", got)
	}
	if _, err := history.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// The trimmed row keeps its short summary text and loses the rest, in the
	// index as well as in the column retention rewrote.
	if got := searchIDs(t, history, "kept-marker"); len(got) != 1 || got[0] != "trimmed" {
		t.Fatalf("after the trim, kept-marker matched %v, want the trimmed row", got)
	}
	if got := searchIDs(t, history, "tail-marker"); len(got) != 0 {
		t.Fatalf("after the trim, tail-marker still matched %v; the index kept dropped text", got)
	}
	if n := ftsRowCount(t, st, "tail-marker"); n != 0 {
		t.Fatalf("the trigram index still holds %d entries for dropped text", n)
	}
	// A row retention deleted never matches again.
	if got := searchIDs(t, history, "expired-marker"); len(got) != 0 {
		t.Fatalf("an expired row still matched %v", got)
	}
	// A row written now is searchable without waiting for a rebuild.
	if err := history.Add(entry("fresh", now, func(e *activity.Entry) {
		e.Transcript = "brand-new-marker right now"
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := searchIDs(t, history, "brand-new-marker"); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("a new row matched %v before any rebuild, want it immediately", got)
	}
}

// TestSearchIndexBackfillIsResumableAndIdempotent covers the upgrade path: a
// database from a build without the index is rebuilt in bounded batches, a
// restart continues from the persisted cursor, and a second pass does nothing.
func TestSearchIndexBackfillIsResumableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	const rows = 1000
	// The marker sits above the first batch, so a search must still find it
	// while the rebuild is partway through.
	history, st := seedWindow(t, rows, 200, map[int]string{900: "resume-marker"})

	first, err := history.WarmSearchIndex(ctx)
	if err != nil {
		t.Fatalf("WarmSearchIndex: %v", err)
	}
	if first.Complete {
		t.Fatal("the first batch reported a complete rebuild for a 1000-row window")
	}
	if first.IndexedThrough == 0 || first.IndexedThrough >= rows {
		t.Fatalf("first batch indexed through %d, want a bounded prefix of %d rows", first.IndexedThrough, rows)
	}
	// Rows above the cursor are not in the index yet; the search must still
	// answer completely instead of silently ignoring them.
	if got := searchIDs(t, history, "resume-marker"); len(got) != 1 {
		t.Fatalf("a search during the rebuild matched %v, want the not-yet-indexed row", got)
	}

	state := warmSearchIndex(t, history)
	if !state.Complete || state.IndexedThrough != rows {
		t.Fatalf("rebuild finished at %+v, want complete through %d", state, rows)
	}
	if n := ftsRows(t, st); n != rows {
		t.Fatalf("the index holds %d rows for %d retained rows", n, rows)
	}
	if got := searchIDs(t, history, "resume-marker"); len(got) != 1 {
		t.Fatalf("after the rebuild, resume-marker matched %v, want exactly one row", got)
	}

	// A second start reads the persisted cursor and does no work.
	again, err := history.WarmSearchIndex(ctx)
	if err != nil {
		t.Fatalf("second WarmSearchIndex: %v", err)
	}
	if !again.Complete || again.IndexedThrough != state.IndexedThrough {
		t.Fatalf("second warmup = %+v, want the same completed state", again)
	}
	if n := ftsRows(t, st); n != rows {
		t.Fatalf("a second warmup left %d index rows, want %d: it duplicated entries", n, rows)
	}

	// A row deleted by retention after the rebuild leaves the index with it.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM activity WHERE request_id = 'req-900'`); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if got := searchIDs(t, history, "resume-marker"); len(got) != 0 {
		t.Fatalf("a deleted row still matched %v", got)
	}
}

// TestNewHistoryDoesNotWaitForTheIndexRebuild covers the startup half of the
// migration note: opening the store and wiring the history must not block on
// tokenizing a whole retained window.
func TestNewHistoryDoesNotWaitForTheIndexRebuild(t *testing.T) {
	st := openStore(t)
	seedRawRows(t, st, largeWindowRows, fillerHaystack)

	started := time.Now()
	history := activity.NewHistory(st.DB(), activity.HistoryOptions{})
	elapsed := time.Since(started)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("NewHistory took %v with %d rows to index, want a background rebuild",
			elapsed, largeWindowRows)
	}
	if history == nil {
		t.Fatal("NewHistory returned nil")
	}
	t.Logf("NewHistory with %d unindexed rows: %v", largeWindowRows, elapsed)
}

// searchIDs runs one search and returns the ids it matched, newest first.
func searchIDs(t *testing.T, history *activity.History, text string) []string {
	t.Helper()
	page, err := history.QueryPage(context.Background(), activity.Query{Text: text, Limit: 100})
	if err != nil {
		t.Fatalf("QueryPage(%q): %v", text, err)
	}
	return ids(page.Entries)
}

func ids(entries []activity.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// ftsRows counts the entries in the trigram index.
func ftsRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM activity_fts`).Scan(&n); err != nil {
		t.Fatalf("count index rows: %v", err)
	}
	return n
}

// ftsRowCount counts the index entries for one needle, which is how a test
// proves dropped text left the index rather than only the column.
func ftsRowCount(t *testing.T, st *store.Store, needle string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM activity_fts WHERE activity_fts MATCH ?`, `"`+needle+`"`).Scan(&n); err != nil {
		t.Fatalf("match index rows: %v", err)
	}
	return n
}

// seedRawRows writes rows into an already open store, before any search schema
// exists, in one statement.
func seedRawRows(t *testing.T, st *store.Store, rows, haystack int) {
	t.Helper()
	var filler strings.Builder
	for filler.Len() < haystack {
		filler.WriteString(fillerWords[filler.Len()%len(fillerWords)])
		filler.WriteByte(' ')
	}
	// The row number keeps each haystack distinct the way a real transcript is,
	// without the cost of building 20,000 separate strings in Go.
	base := time.Now().Add(-time.Duration(rows) * time.Minute).UnixMilli()
	if _, err := st.DB().Exec(`
		INSERT INTO activity (request_id, at, source, path, tool, outcome, search)
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ? - 1)
		SELECT 'req-' || i, ? + i * 60000, 'voice', 'fast_path',
			'calendar.get_events', 'ok', ? || ' ' || i
		FROM n`, rows, base, filler.String()); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
}
