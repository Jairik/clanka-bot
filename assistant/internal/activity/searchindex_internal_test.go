package activity

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// TestNoMatchSearchPlanUsesTheIndex is the query-plan half of #210: the
// statement QueryPage actually builds for a query that matches nothing must be
// answered from activity_fts, not by walking activity.
//
// It is an internal test on purpose: it composes the exact predicate
// searchCondition returns, so the assertion cannot drift from the shipped SQL.
func TestNoMatchSearchPlanUsesTheIndex(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	history := NewHistory(st.DB(), HistoryOptions{SkipSearchWarmup: true})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		if err := history.Add(Entry{
			ID:         "req-" + strings.Repeat("x", i+1),
			At:         base.Add(time.Duration(i) * time.Minute),
			Source:     "voice",
			Path:       "fast_path",
			Tool:       "calendar.get_events",
			Outcome:    "ok",
			Transcript: "what is on today",
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

	// A needle the window does not contain, which is the case the issue is
	// about: today's code has to read every haystack before it can answer.
	condition, args, err := history.searchCondition(ctx, "zzzz-not-present")
	if err != nil {
		t.Fatalf("searchCondition: %v", err)
	}
	statement := `SELECT ` + entryColumns + ` FROM activity WHERE ` + condition +
		` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, 26)

	plan := queryPlan(t, st.DB(), statement, args...)
	if !strings.Contains(plan, "activity_fts") {
		t.Fatalf("no-match search plan does not touch the trigram index:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN activity USING INDEX activity_at") {
		t.Fatalf("no-match search still walks the retained window:\n%s", plan)
	}

	// The windowed fallback for one and two character queries must bound the
	// rows it reads: MATERIALIZE applies the LIMIT before the haystack filter,
	// so the filter cannot turn it back into a window scan.
	if !history.windowedSearch(ctx, "zz") {
		t.Fatal("a two character query did not take the bounded fallback")
	}
	if history.windowedSearch(ctx, "zzz") {
		t.Fatal("a three character query took the fallback instead of the index")
	}
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
