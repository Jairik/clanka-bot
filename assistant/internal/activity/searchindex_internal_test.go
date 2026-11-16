package activity

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// TestSearchPlansAreBounded is the query-plan half of #210. The indexed page
// must read a bounded candidate chunk from activity_fts and never ask SQLite to
// materialize the whole match set; the dense-needle probe must read a bounded
// window through activity_at. Both statements are the ones the shipped code
// runs, so the assertion cannot drift from it.
func TestSearchPlansAreBounded(t *testing.T) {
	history, st := searchFixture(t, 40)

	plan := queryPlan(t, st.DB(), searchChunkSQL, ftsPhrase("the weather"), int64(1<<62), searchPageChunk)
	if !strings.Contains(plan, "activity_fts") {
		t.Fatalf("candidate chunk does not touch the trigram index:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN activity ") {
		t.Fatalf("candidate chunk walks the activity table:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("candidate chunk sorts the match set:\n%s", plan)
	}

	plan = queryPlan(t, st.DB(), probeWindowSQL(nil), 26, "the weather", 26)
	if !strings.Contains(plan, "activity_at") {
		t.Fatalf("dense-needle probe does not read the newest rows through activity_at:\n%s", plan)
	}
	if strings.Contains(plan, "activity_fts") {
		t.Fatalf("dense-needle probe goes through the index instead of an early-stopping read:\n%s", plan)
	}
	if !strings.Contains(plan, "SEARCH activity USING INDEX activity_at") {
		t.Fatalf("dense-needle probe is not bounded to a time band on activity_at:\n%s", plan)
	}

	// The bounded fallback for one and two character queries must bound the
	// rows it reads: MATERIALIZE applies the LIMIT before the haystack filter,
	// so the filter cannot turn it back into a window scan.
	plan = queryPlan(t, st.DB(), windowedSearchSQL(nil), ShortSearchWindow, "zz", 26)
	if !strings.Contains(plan, "MATERIALIZE") {
		t.Fatalf("short-query fallback does not materialize its window:\n%s", plan)
	}

	_ = history
}

// TestUnavailableIndexStillAnswersAndSaysSo covers the degraded build: when the
// index cannot be created, a long query still answers from the bounded window
// and the page is told the answer only covers that window.
func TestUnavailableIndexStillAnswersAndSaysSo(t *testing.T) {
	ctx := context.Background()
	history, _ := searchFixture(t, 40)
	// A build without FTS5 fails the DDL; the shipped code remembers that
	// failure for the process, which is what this test injects.
	history.schemaMu.Lock()
	history.schemaReady = false
	history.schemaErr = context.Canceled
	history.schemaMu.Unlock()

	page, err := history.QueryPage(ctx, Query{Text: "the weather", Limit: 10})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if page.SearchWindow != SearchWindowIndexUnavailable {
		t.Fatalf("search window = %q, want the unavailable-index notice", page.SearchWindow)
	}
	if len(page.Entries) == 0 {
		t.Fatal("the bounded window answered nothing for a needle it holds")
	}
}

// searchFixture opens a real store with n indexed rows whose haystack contains
// "the weather".
func searchFixture(t *testing.T, n int) (*History, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	history := NewHistory(st.DB(), HistoryOptions{SkipSearchWarmup: true})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if err := history.Add(Entry{
			ID:         "req-" + strings.Repeat("x", i+1),
			At:         base.Add(time.Duration(i) * time.Minute),
			Source:     "voice",
			Path:       "fast_path",
			Tool:       "calendar.get_events",
			Outcome:    "ok",
			Transcript: "what is the weather today",
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	for {
		state, err := history.WarmSearchIndex(ctx)
		if err != nil {
			t.Fatalf("WarmSearchIndex: %v", err)
		}
		if state.Complete {
			break
		}
	}
	return history, st
}

// queryPlan returns the EXPLAIN QUERY PLAN detail lines joined for one
// statement, so a test can assert which index answered it.
func queryPlan(t *testing.T, db *sql.DB, statement string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var (
			id, parent, unused int
			detail             string
		)
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return strings.Join(lines, "\n")
}
