package activity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// openHistory opens the real assistant database in a temp directory and wraps
// its handle, so these tests exercise the migration and the real driver.
func openHistory(t *testing.T, opts activity.HistoryOptions) (*activity.History, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return activity.NewHistory(st.DB(), opts), st
}

func entry(id string, at time.Time, over func(*activity.Entry)) activity.Entry {
	e := activity.Entry{
		ID:         id,
		At:         at,
		Source:     "voice",
		Path:       "fast_path",
		Tool:       "calendar.get_events",
		Outcome:    "ok",
		Transcript: "what is on today",
		Reply:      "Nothing until four.",
		Stages:     []activity.Timing{{Name: "routing", MS: 40}},
	}
	if over != nil {
		over(&e)
	}
	return e
}

func TestHistorySurvivesARestartAndPagesNewestFirst(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/data/assistant.db"
	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	history := activity.NewHistory(first.DB(), activity.HistoryOptions{})
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := history.Add(entry("req-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour), nil)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A restart opens the same file: a request from last week is still there.
	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	history = activity.NewHistory(second.DB(), activity.HistoryOptions{})
	page, err := history.QueryPage(ctx, activity.Query{Limit: 10})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("entries = %d after restart, want 3", len(page.Entries))
	}
	if page.Entries[0].ID != "req-c" || page.Entries[2].ID != "req-a" {
		t.Fatalf("order = %q..%q, want newest first", page.Entries[0].ID, page.Entries[2].ID)
	}
	if got := page.Entries[0].Transcript; got != "what is on today" {
		t.Errorf("transcript after restart = %q", got)
	}
	if page.Entries[0].Cursor == "" {
		t.Error("durable entry has no cursor")
	}
}

func TestHistoryPagingDoesNotDuplicateOrSkipRows(t *testing.T) {
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		// Same millisecond for two rows: the cursor still needs a stable tie
		// break, which is the row id.
		at := base.Add(time.Duration(i/2) * time.Minute)
		if err := history.Add(entry("req-"+string(rune('a'+i)), at, nil)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	seen := map[string]bool{}
	var cursor string
	for page := 0; page < 5; page++ {
		result, err := history.QueryPage(ctx, activity.Query{Limit: 3, Before: cursor})
		if err != nil {
			t.Fatalf("QueryPage(%d): %v", page, err)
		}
		for _, e := range result.Entries {
			if seen[e.ID] {
				t.Fatalf("entry %q returned twice across pages", e.ID)
			}
			seen[e.ID] = true
		}
		cursor = result.Next
		if cursor == "" {
			break
		}
		// A row arriving between pages must not shift the cursor.
		if page == 0 {
			if err := history.Add(entry("req-new", base.Add(time.Hour), nil)); err != nil {
				t.Fatalf("Add while paging: %v", err)
			}
		}
	}
	if len(seen) != 7 {
		t.Fatalf("saw %d distinct entries, want 7", len(seen))
	}
	if before, err := history.QueryPage(ctx, activity.Query{Before: "not-a-cursor"}); err == nil || before.Entries != nil {
		t.Fatal("a malformed cursor was accepted")
	}
}

func TestHistorySearchCoversTheFieldsThePageShows(t *testing.T) {
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
	}
	for _, e := range entries {
		if err := history.Add(e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	cases := []struct {
		name   string
		filter string
		text   string
		want   []string
	}{
		{"transcript", "all", "blue folder", []string{"transcript"}},
		{"reply", "all", "kettle", []string{"reply"}},
		{"tool name", "all", "notes.search", []string{"tool"}},
		{"humanized path", "all", "agent path", []string{"path"}},
		{"raw path", "all", "fast_path", []string{"error", "tool", "reply", "transcript"}},
		{"error text", "all", "timed out", []string{"error"}},
		{"problems filter", "problems", "", []string{"error"}},
		{"voice filter excludes typed", "voice", "blue folder", nil},
		{"no match", "all", "zzzz", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := history.QueryPage(ctx, activity.Query{Filter: tc.filter, Text: tc.text, Limit: 20})
			if err != nil {
				t.Fatalf("QueryPage: %v", err)
			}
			got := make([]string, len(page.Entries))
			for i, e := range page.Entries {
				got[i] = e.ID
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
			// The ring-only matcher must agree with the SQL one.
			for _, e := range entries {
				inSQL := false
				for _, id := range tc.want {
					inSQL = inSQL || id == e.ID
				}
				if e.Matches(tc.filter, tc.text) != inSQL {
					t.Errorf("%s: ring match = %v, SQL match = %v", e.ID, e.Matches(tc.filter, tc.text), inSQL)
				}
			}
		})
	}
	if _, err := history.QueryPage(ctx, activity.Query{Filter: "nonsense"}); err == nil {
		t.Fatal("unknown filter was accepted")
	}
}

func TestHistorySearchIsBounded(t *testing.T) {
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	if err := history.Add(entry("long", at, func(e *activity.Entry) {
		e.Transcript = strings.Repeat("x", 9000) + " needle"
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Beyond the capped haystack the word is not kept, so one row cannot force
	// an unbounded scan.
	page, err := history.QueryPage(ctx, activity.Query{Text: "needle"})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("matches = %d, want 0 beyond the search cap", len(page.Entries))
	}
	hello := strings.Repeat("y", 200) + " needle"
	if err := history.Add(entry("short", at.Add(time.Minute), func(e *activity.Entry) { e.Transcript = hello })); err != nil {
		t.Fatalf("Add: %v", err)
	}
	page, err = history.QueryPage(ctx, activity.Query{Text: strings.Repeat("z", 500) + " needle"})
	if err != nil {
		t.Fatalf("QueryPage with a long pattern: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatal("a pasted long query returned a row")
	}
}

func TestPruneTrimsOldTextAndDeletesExpiredRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	history, _ := openHistory(t, activity.HistoryOptions{
		FullWindow: 24 * time.Hour,
		MaxAge:     7 * 24 * time.Hour,
		MaxRows:    100,
		Now:        func() time.Time { return now },
	})
	long := strings.Repeat("a long sentence ", 40)
	entries := []activity.Entry{
		entry("expired", now.Add(-8*24*time.Hour), func(e *activity.Entry) { e.Transcript = long }),
		entry("trimmed", now.Add(-2*24*time.Hour), func(e *activity.Entry) {
			e.Transcript = long
			e.Reply = long
			e.Tool = "notes.add"
			e.Calls = []activity.Call{{Name: "notes.add", LocalOnly: true, Result: long}}
		}),
		entry("recent-1", now.Add(-time.Hour), func(e *activity.Entry) { e.Transcript = long }),
		entry("recent-2", now.Add(-2*time.Hour), nil),
		entry("recent-3", now.Add(-3*time.Hour), nil),
		entry("recent-4", now.Add(-4*time.Hour), nil),
	}
	for _, e := range entries {
		if err := history.Add(e); err != nil {
			t.Fatalf("Add %s: %v", e.ID, err)
		}
	}

	result, err := history.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if result.Deleted != 1 || result.Trimmed != 1 || result.Remaining != 5 {
		t.Fatalf("prune = %+v, want one expired delete, one trim and five rows", result)
	}
	page, err := history.QueryPage(ctx, activity.Query{Limit: 100})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	var trimmed *activity.Entry
	for i := range page.Entries {
		if page.Entries[i].ID == "trimmed" {
			trimmed = &page.Entries[i]
		}
		if page.Entries[i].ID == "recent-1" && page.Entries[i].Transcript != long {
			t.Error("a row inside the full window lost its transcript")
		}
	}
	if trimmed == nil {
		t.Fatal("the two-day-old row was deleted instead of trimmed")
	}
	if !trimmed.Summary {
		t.Error("trimmed row is not marked as a summary")
	}
	if n := len([]rune(trimmed.Transcript)); n > 200 {
		t.Errorf("trimmed transcript = %d runes, want a bounded summary", n)
	}
	if len(trimmed.Calls) != 1 || trimmed.Calls[0].Name != "notes.add" || trimmed.Calls[0].Result != "" {
		t.Fatalf("trimmed calls = %+v, want the name kept and the result dropped", trimmed.Calls)
	}
	if trimmed.Calls[0].LocalOnly != true {
		t.Error("trimmed call lost its Local-only marker")
	}
	// A second pass is a no-op, so the hourly tick cannot churn the file.
	again, err := history.Prune(ctx)
	if err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	if again.Trimmed != 0 || again.Deleted != 0 {
		t.Fatalf("second prune = %+v, want no changes", again)
	}
	// The trimmed text is still searchable, the dropped text is not.
	for _, tc := range []struct {
		text string
		want int
	}{{"a long sentence", 2}, {strings.Repeat("a long sentence ", 30), 0}} {
		got, err := history.QueryPage(ctx, activity.Query{Text: tc.text, Limit: 10})
		if err != nil {
			t.Fatalf("QueryPage: %v", err)
		}
		if len(got.Entries) != tc.want {
			t.Errorf("search %q matched %d rows, want %d", tc.text, len(got.Entries), tc.want)
		}
	}
}

func TestPruneCapsTheRowCountIndependentlyOfAge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	history, _ := openHistory(t, activity.HistoryOptions{
		FullWindow: 24 * time.Hour,
		MaxAge:     30 * 24 * time.Hour,
		MaxRows:    2,
		Now:        func() time.Time { return now },
	})
	for i := 0; i < 5; i++ {
		if err := history.Add(entry("req-"+string(rune('a'+i)), now.Add(-time.Duration(i)*time.Minute), nil)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	result, err := history.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if result.Deleted != 3 || result.Remaining != 2 {
		t.Fatalf("prune = %+v, want the oldest three deleted and two kept", result)
	}
	page, err := history.QueryPage(ctx, activity.Query{Limit: 10})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if len(page.Entries) != 2 || page.Entries[0].ID != "req-a" || page.Entries[1].ID != "req-b" {
		t.Fatalf("kept %+v, want the two newest", page.Entries)
	}
}

func TestRingKeepsWorkingAndWritesTheHistory(t *testing.T) {
	ctx := context.Background()
	history, _ := openHistory(t, activity.HistoryOptions{})
	ring := activity.NewWithHistory(2, history)
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		ring.Observe(activityCompleted("req-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Minute)))
	}
	if ring.Len() != 2 {
		t.Fatalf("ring len = %d, want the bounded 2", ring.Len())
	}
	page, err := history.QueryPage(ctx, activity.Query{Limit: 10})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if len(page.Entries) != 4 {
		t.Fatalf("history rows = %d, want every completed request", len(page.Entries))
	}
	if page.Entries[3].ID != "req-a" {
		t.Fatalf("oldest durable row = %q, want req-a", page.Entries[3].ID)
	}
	if ring.History() != history {
		t.Fatal("Store.History did not return the attached history")
	}
}

// activityCompleted is the completion event the latency observer delivers.
func activityCompleted(id string, at time.Time) latency.Completed {
	return latency.Completed{
		ID:         id,
		Source:     latency.SourceVoice,
		Path:       "fast_path",
		Tool:       "calendar.get_events",
		Transcript: "what is on today",
		Outcome:    latency.OutcomeOK,
		Detail:     &latency.Detail{Reply: "nothing until four"},
	}
}

func TestHistoryWritePathStaysBounded(t *testing.T) {
	history, _ := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	started := time.Now()
	for i := 0; i < 200; i++ {
		if err := history.Add(entry("req-"+strings.Repeat("i", 4), base.Add(time.Duration(i)*time.Millisecond), nil)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// 200 inserts on the assistant's single writer; a scan or a rebuild of the
	// haystack on every write would be far slower than this ceiling.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("200 history writes took %v, want a bounded write path", elapsed)
	}
}
