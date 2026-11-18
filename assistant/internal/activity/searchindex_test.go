package activity_test

import (
	"context"
	"fmt"
	"math"
	"sort"
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
	// searchCeiling is the acceptance bound for one search over the whole
	// retained window. The scan it replaces took 53-88 ms locally at 1 KB
	// haystacks; an indexed answer is well under a millisecond, so this leaves
	// room for a loaded CI machine and still fails a reintroduced window scan.
	// raceSlowdown (race_on_test.go) scales it under the race detector, which
	// slows the driver by an order of magnitude.
	searchCeiling = 20 * time.Millisecond * raceSlowdown
	// writeCeiling is the 99th-percentile bound for one history insert with the
	// index attached. Without a merge policy this was 48.7 ms, because FTS5
	// merged segments on the goroutine that also holds the only connection; the
	// explicit policy keeps the burst off the write path entirely.
	writeCeiling = 20 * time.Millisecond * raceSlowdown
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

// TestSearchIsBoundedOnTheRetainedWindow is the #210 acceptance test: both the
// absent needle (the old scan's worst case) and the needle that matches most of
// the window (the indexed read's worst case) answer well inside the bound, a
// real match still returns the same rows, and the cost does not grow with the
// window.
func TestSearchIsBoundedOnTheRetainedWindow(t *testing.T) {
	skipScaleUnderRace(t)
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

	// The scan it replaced stopped early for a needle that matches almost every
	// row, so the indexed read must not give that back: a page materializing
	// every match and all sixteen columns took 88 ms here (#373 review).
	t.Run("a match-most search is not slower than the scan", func(t *testing.T) {
		elapsed, matched := bestQueryTime(t, history, "the", 20)
		if matched != 25 {
			t.Fatalf("match-most search returned %d rows, want a full page", matched)
		}
		if elapsed > searchCeiling {
			t.Fatalf("match-most search over %d rows took %v, want under %v",
				largeWindowRows, elapsed, searchCeiling)
		}
		t.Logf("match-most search over %d rows: %v (%d rows)", largeWindowRows, elapsed, matched)
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
		// held the connection for 53-88 ms per search, so a write could wait a
		// whole scan; the ceiling is a wide multiple of a real insert and still
		// catches that.
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
	warmSearchIndex(t, history)
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

// TestSearchMatchesTheRingRuleAcrossFiltersAndPages is the broad equivalence
// check for the page machinery: every filter and a range of query densities,
// paged to exhaustion with a Small limit, must return exactly the ring rule's
// rows in exactly its order. It exercises the dense probe, the chunked index
// read and the cursor together, which is where a bounded candidate set could
// silently drop a row.
func TestSearchMatchesTheRingRuleAcrossFiltersAndPages(t *testing.T) {
	skipScaleUnderRace(t)
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	sources := []string{"voice", "dashboard", "debug", "dashboard_mic"}
	const total = 400
	entries := make([]activity.Entry, 0, total)
	for i := 0; i < total; i++ {
		text := fmt.Sprintf("the folder report number %d", i)
		outcome := "ok"
		failure := ""
		switch {
		case i%7 == 0:
			text = "the kettle boiled over rare-marker"
		case i%11 == 0:
			text = "the weather needle in the weekend forecast"
		}
		if i%13 == 0 {
			outcome = "failed"
			failure = "weather provider timed out"
		}
		e := entry(fmt.Sprintf("req-%03d", i), base.Add(time.Duration(i)*time.Minute), func(e *activity.Entry) {
			e.Source = sources[i%len(sources)]
			e.Transcript = text
			e.Outcome = outcome
			e.Error = failure
			if i%5 == 0 {
				e.Tool = "notes.search"
			}
		})
		if err := history.Add(e); err != nil {
			t.Fatalf("Add: %v", err)
		}
		entries = append(entries, e)
	}
	warmSearchIndex(t, history)

	for _, filter := range []string{"all", "voice", "typed", "mic", "problems"} {
		for _, needle := range []string{"", "the", "kettle", "weather", "eath", "ra", "zzzz-not-present"} {
			var want []string
			// The ring rule is newest first, which is the reverse of the seed.
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Matches(filter, needle) {
					want = append(want, entries[i].ID)
				}
			}
			var seen []string
			cursor := ""
			for page := 0; page < 2*total; page++ {
				result, err := history.QueryPage(ctx, activity.Query{
					Filter: filter, Text: needle, Limit: 7, Before: cursor,
				})
				if err != nil {
					t.Fatalf("QueryPage(filter=%q, text=%q): %v", filter, needle, err)
				}
				for _, e := range result.Entries {
					seen = append(seen, e.ID)
				}
				cursor = result.Next
				if cursor == "" {
					break
				}
			}
			if strings.Join(seen, ",") != strings.Join(want, ",") {
				t.Fatalf("filter=%q text=%q matched %d rows (%v...), ring rule has %d (%v...)",
					filter, needle, len(seen), head(seen, 5), len(want), head(want, 5))
			}
		}
	}
}

func head(ids []string, n int) []string {
	if len(ids) < n {
		return ids
	}
	return ids[:n]
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
	warmSearchIndex(t, history)
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
		{"match-most", "the"},
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
			if tc.name == "match-most" {
				if len(seen) != total {
					t.Fatalf("paged search for %q returned %d rows, want %d", tc.text, len(seen), total)
				}
				return
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

// TestShortQueryReadsOnlyTheNewestWindow covers the documented fallback for one
// and two character queries, which a trigram index cannot answer: it must not
// error, it must not walk the retained window, and the page must be told the
// answer only covered that window.
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
		if page.SearchWindow != activity.SearchWindowShort {
			t.Fatalf("search %q reported window %q, want the short-query notice", needle, page.SearchWindow)
		}
	}
	// Three characters reach the index, so the older row matches again and the
	// answer is complete.
	page, err := history.QueryPage(ctx, activity.Query{Text: "zeb", Limit: 50})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if got := ids(page.Entries); len(got) != 2 || got[0] != "req-250" || got[1] != "req-050" {
		t.Fatalf("three-character search matched %v, want both zebra rows newest first", got)
	}
	if page.SearchWindow != activity.SearchWindowNone {
		t.Fatalf("an indexed search reported window %q, want a complete answer", page.SearchWindow)
	}
}

// TestSearchIndexFollowsTrimAndDelete covers the writer half: the index must
// carry a trimmed row's kept text, drop the text retention removed, and index a
// new row immediately.
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
	ftsIntegrity(t, st)
}

// TestPruneSurvivesAPartiallyIndexedUpgrade is the regression test for the
// SQLITE_CORRUPT blocker: an upgraded database has rows the rebuild has not
// reached, and the first Prune after the upgrade deletes and trims rows above
// the backfill cursor. The old external-content design raised "database disk
// image is malformed (267)" on exactly this statement, because a trigger
// retracted an index entry that did not exist yet. The explicit-maintenance
// design must survive it and leave both tables agreeing.
func TestPruneSurvivesAPartiallyIndexedUpgrade(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	const rows = 900
	seedRawRows(t, st, rows, 400)
	markers := map[int]string{
		10:  "deleted-indexed-marker",
		850: "above-cursor-marker",
	}
	for i, marker := range markers {
		if _, err := st.DB().Exec(`
			UPDATE activity SET search = ? || ' ' || search WHERE request_id = ?`,
			marker, fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatalf("mark row %d: %v", i, err)
		}
	}
	// The upgrade shape: rows exist, and only the first batch is indexed.
	last := time.Now().Add(-time.Minute).UnixMilli()
	history := activity.NewHistory(st.DB(), activity.HistoryOptions{
		FullWindow:       time.Hour,
		MaxAge:           30 * 24 * time.Hour,
		MaxRows:          rows * 5 / 9, // the cap deletes ids 1..400, straddling the cursor
		SkipSearchWarmup: true,
		Now:              func() time.Time { return time.UnixMilli(last) },
	})
	first, err := history.WarmSearchIndex(ctx)
	if err != nil {
		t.Fatalf("WarmSearchIndex: %v", err)
	}
	if first.Complete || first.IndexedThrough != 200 {
		t.Fatalf("first batch = %+v, want 200 rows indexed and an incomplete rebuild", first)
	}
	if n := ftsRows(t, st); n != 200 {
		t.Fatalf("index holds %d rows after one batch, want 200", n)
	}

	// This is the statement that used to fail: a row cap that deletes indexed
	// and un-indexed rows together, followed by a trim of rows the rebuild has
	// not reached.
	result, err := history.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune on a partly indexed upgrade raised %v", err)
	}
	if result.Deleted != 400 || result.Remaining != 500 {
		t.Fatalf("prune = %+v, want 400 deleted and 500 kept", result)
	}
	// The kept rows are above the cursor; the search still finds them through
	// the un-indexed tail while the rebuild continues.
	if got := searchIDs(t, history, "above-cursor-marker"); len(got) != 1 || got[0] != "req-850" {
		t.Fatalf("a row above the cursor matched %v", got)
	}
	if got := searchIDs(t, history, "deleted-indexed-marker"); len(got) != 0 {
		t.Fatalf("a deleted indexed row still matched %v", got)
	}
	ftsIntegrity(t, st)

	// A mixed DELETE is the other half of the repro: ids 1..200 were indexed,
	// 201..600 were not, and both ranges are already gone above.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM activity WHERE id <= 450`); err != nil {
		t.Fatalf("mixed delete across the cursor: %v", err)
	}
	ftsIntegrity(t, st)

	// Finishing the rebuild converges the index on what is left.
	state := warmSearchIndex(t, history)
	if !state.Complete {
		t.Fatalf("rebuild did not finish: %+v", state)
	}
	if n := ftsRows(t, st); n != 450 {
		t.Fatalf("index holds %d rows for the 450 retained rows", n)
	}
	ftsIntegrity(t, st)
}

// TestSearchIndexUpgradesFromTheTriggerDesign opens a database written by the
// previous design on this branch: an external-content table, the three sync
// triggers and a state table without the new columns. The upgrade must drop the
// triggers, replace the table with the content-bearing one, reset the state and
// rebuild, so the corrupting shape cannot survive an upgrade.
func TestSearchIndexUpgradesFromTheTriggerDesign(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRawRows(t, st, 300, 200)
	for _, statement := range []string{
		`CREATE VIRTUAL TABLE activity_fts USING fts5(search, content='activity', content_rowid='id', tokenize='trigram')`,
		`CREATE TRIGGER activity_fts_delete AFTER DELETE ON activity BEGIN
			INSERT INTO activity_fts (activity_fts, rowid, search) VALUES ('delete', old.id, old.search);
		END`,
		`CREATE TRIGGER activity_fts_update AFTER UPDATE OF search ON activity BEGIN
			INSERT INTO activity_fts (activity_fts, rowid, search) VALUES ('delete', old.id, old.search);
			INSERT INTO activity_fts (rowid, search) VALUES (new.id, new.search);
		END`,
		`CREATE TABLE activity_search_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			indexed_through INTEGER NOT NULL DEFAULT 0,
			complete INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO activity_search_state (id, indexed_through, complete) VALUES (1, 100, 0)`,
		`INSERT INTO activity_fts (rowid, search) SELECT id, search FROM activity WHERE id <= 100`,
	} {
		if _, err := st.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed old design: %v", err)
		}
	}
	history := activity.NewHistory(st.DB(), activity.HistoryOptions{SkipSearchWarmup: true})
	if state := warmSearchIndex(t, history); !state.Complete || state.IndexedThrough != 300 {
		t.Fatalf("upgraded rebuild = %+v, want complete through 300", state)
	}
	var triggers int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'activity_fts%'`).Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 0 {
		t.Fatalf("%d sync triggers survived the upgrade", triggers)
	}
	var sql string
	if err := st.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'activity_fts'`).Scan(&sql); err != nil {
		t.Fatalf("read definition: %v", err)
	}
	if strings.Contains(strings.ToLower(sql), "content=") {
		t.Fatalf("upgraded index is still external content: %s", sql)
	}
	if n := ftsRows(t, st); n != 300 {
		t.Fatalf("index holds %d rows for 300 retained rows", n)
	}
	ftsIntegrity(t, st)
	if _, err := history.Prune(ctx); err != nil {
		t.Fatalf("Prune after the upgrade: %v", err)
	}
}

// TestSearchIndexBackfillResumesAfterACrash covers the crash half of the
// blocker: a committed batch survives, the next start resumes from its cursor,
// and no trigger can disagree with the index because there are none.
func TestSearchIndexBackfillResumesAfterACrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/data/assistant.db"

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	seedRawRows(t, first, 1000, 200)
	if _, err := first.DB().Exec(`
		UPDATE activity SET search = 'resume-marker ' || search WHERE request_id = 'req-900'`); err != nil {
		t.Fatalf("mark row: %v", err)
	}
	history := activity.NewHistory(first.DB(), activity.HistoryOptions{SkipSearchWarmup: true})
	state, err := history.WarmSearchIndex(ctx)
	if err != nil {
		t.Fatalf("WarmSearchIndex: %v", err)
	}
	if state.Complete || state.IndexedThrough != 200 {
		t.Fatalf("first batch = %+v, want a bounded prefix", state)
	}
	// A crash right here leaves exactly this: one committed batch.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	history = activity.NewHistory(second.DB(), activity.HistoryOptions{SkipSearchWarmup: true})
	var indexed, complete int
	if err := second.DB().QueryRow(`SELECT indexed_through, complete FROM activity_search_state WHERE id = 1`).
		Scan(&indexed, &complete); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if indexed != 200 || complete != 0 {
		t.Fatalf("state after restart = (%d, %d), want (200, 0)", indexed, complete)
	}
	// The index never holds a row above the cursor, so there is nothing a
	// trigger could retract and nothing a search could double-count.
	var above int
	if err := second.DB().QueryRow(`SELECT count(*) FROM activity_fts WHERE rowid > ?`, indexed).
		Scan(&above); err != nil {
		t.Fatalf("count index rows: %v", err)
	}
	if above != 0 {
		t.Fatalf("index holds %d rows above the cursor after a crash", above)
	}
	if got := searchIDs(t, history, "resume-marker"); len(got) != 1 {
		t.Fatalf("a search mid-rebuild matched %v, want the un-indexed row", got)
	}
	// The resumed rebuild finishes, and every row is indexed once.
	if state := warmSearchIndex(t, history); !state.Complete || state.IndexedThrough != 1000 {
		t.Fatalf("resumed rebuild = %+v", state)
	}
	if n := ftsRows(t, second); n != 1000 {
		t.Fatalf("index holds %d rows for 1000 retained rows", n)
	}
	if got := searchIDs(t, history, "resume-marker"); len(got) != 1 {
		t.Fatalf("resume-marker matched %v after the rebuild, want exactly one row", got)
	}
	ftsIntegrity(t, second)
}

// TestSearchIndexBackfillIsIdempotent re-runs a committed batch against an
// index that already holds every row. The index must not grow and must stay
// internally consistent; count(*) is a real count here because the table
// carries its own content.
func TestSearchIndexBackfillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	const rows = 1000
	history, st := seedWindow(t, rows, 200, map[int]string{900: "resume-marker"})
	state := warmSearchIndex(t, history)
	if !state.Complete || state.IndexedThrough != rows {
		t.Fatalf("rebuild finished at %+v, want complete through %d", state, rows)
	}
	if n := ftsRows(t, st); n != rows {
		t.Fatalf("the index holds %d rows for %d retained rows", n, rows)
	}
	ftsIntegrity(t, st)
	// Rewind the cursor without clearing the index: the next pass replays
	// batches it has already written.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE activity_search_state SET indexed_through = 500, complete = 0 WHERE id = 1`); err != nil {
		t.Fatalf("rewind cursor: %v", err)
	}
	if state := warmSearchIndex(t, history); !state.Complete {
		t.Fatalf("replayed rebuild = %+v, want complete", state)
	}
	if n := ftsRows(t, st); n != rows {
		t.Fatalf("a replayed batch left %d index rows, want %d: it duplicated entries", n, rows)
	}
	if got := searchIDs(t, history, "resume-marker"); len(got) != 1 {
		t.Fatalf("resume-marker matched %v after the replay, want exactly one row", got)
	}
	ftsIntegrity(t, st)
}

// TestSearchIndexIsReclaimedAfterATrim covers the growth half: retention
// rewrites every trimmed haystack to a short summary, and the maintenance step
// must return the dead postings to SQLite rather than ratchet the file to the
// peak haystack. The test measures the bytes the index's shadow tables actually
// occupy (dbstat) and the database's free pages.
func TestSearchIndexIsReclaimedAfterATrim(t *testing.T) {
	skipScaleUnderRace(t)
	ctx := context.Background()
	const rows = 4000
	st := openStore(t)
	seedRawRows(t, st, rows, 400)
	now := time.Now().Truncate(time.Minute)
	history := activity.NewHistory(st.DB(), activity.HistoryOptions{
		FullWindow:       time.Hour,
		MaxAge:           30 * 24 * time.Hour,
		MaxRows:          rows * 2,
		SkipSearchWarmup: true,
		Now:              func() time.Time { return now },
	})
	// The seeded rows end one minute ago and span ~67 hours, so most of them
	// are older than the full window and get trimmed.
	warmSearchIndex(t, history)
	ftsBytesBefore := ftsBytes(t, st)
	if ftsBytesBefore == 0 {
		t.Skip("this build has no dbstat virtual table")
	}

	// Prune trims at most pruneBatch rows per call.
	for i := 0; i < 10; i++ {
		result, err := history.Prune(ctx)
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if result.Trimmed == 0 {
			break
		}
	}
	ftsBytesAfter := ftsBytes(t, st)
	t.Logf("index bytes: %d before trim, %d after; pages=%d freelist=%d",
		ftsBytesBefore, ftsBytesAfter, pageCount(t, st), freelist(t, st))
	if ftsBytesAfter >= ftsBytesBefore {
		t.Fatalf("the index occupies %d bytes after trimming, was %d: the trim did not reclaim it",
			ftsBytesAfter, ftsBytesBefore)
	}
	pageCount(t, st)
	if freelist(t, st) == 0 {
		t.Fatal("trimming left no free pages for the next writes: the file will ratchet")
	}
	// The trimmed rows still match by what they keep (the Tool name), and the
	// long haystack text is gone from the index.
	if got := searchIDs(t, history, "calendar.get_events"); len(got) == 0 {
		t.Fatal("a trimmed row lost its kept summary text in the index")
	}
	ftsIntegrity(t, st)
}

// TestSearchIndexKeepsTheWritePathBounded measures the p99 of a history insert
// with the index attached. FTS5's default merge policy put a segment merge on
// the request goroutine (48.7 ms p99 at 20,000 rows, #373 review); the explicit
// policy moves that work to the maintenance path, so the p99 stays close to the
// index-free write it replaces.
func TestSearchIndexKeepsTheWritePathBounded(t *testing.T) {
	skipScaleUnderRace(t)
	history, _ := seedWindow(t, largeWindowRows, fillerHaystack, nil)
	warmSearchIndex(t, history)
	base := time.Now().Add(time.Duration(largeWindowRows) * time.Minute)
	// Warm the statement cache and the page cache first; the first insert into
	// a freshly built index pays a one-time flush.
	for i := 0; i < 20; i++ {
		if err := history.Add(entry(fmt.Sprintf("warm-%d", i), base.Add(time.Duration(i)*time.Second), nil)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	const writes = 200
	durations := make([]time.Duration, 0, writes)
	for i := 0; i < writes; i++ {
		started := time.Now()
		if err := history.Add(entry(fmt.Sprintf("new-%d", i), base.Add(time.Duration(i)*time.Second), nil)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p99 := durations[98*writes/100]
	if p99 > writeCeiling {
		t.Fatalf("p99 history insert took %v, want under %v: a merge is running on the write path",
			p99, writeCeiling)
	}
	t.Logf("history insert with the index: p50=%v p90=%v p99=%v max=%v",
		durations[writes/2], durations[90*writes/100], p99, durations[writes-1])
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

// ftsRows counts the rows in the trigram index. The table carries its own
// content, so this is a real row count, not activity's.
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

// ftsIntegrity runs FTS5's internal consistency check, which for a
// content-bearing table also compares the index with the stored content.
func ftsIntegrity(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO activity_fts (activity_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("index integrity check: %v", err)
	}
}

// ftsBytes is the on-disk size of the index's shadow tables.
func ftsBytes(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var n int64
	err := st.DB().QueryRow(
		`SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name LIKE 'activity_fts%'`).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

func freelist(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`PRAGMA freelist_count`).Scan(&n); err != nil {
		t.Fatalf("freelist_count: %v", err)
	}
	return n
}

func pageCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`PRAGMA page_count`).Scan(&n); err != nil {
		t.Fatalf("page_count: %v", err)
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

// skipScaleUnderRace skips a scale test under the race detector. These tests
// seed thousands of rows through the pure-Go SQLite driver, which the detector
// slows about 27x, and they check size and timing bounds rather than
// concurrency. CI runs this package a second time without -race, so they still
// gate every change.
func skipScaleUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("scale test; runs in the no-race pass (go test ./internal/activity)")
	}
}
