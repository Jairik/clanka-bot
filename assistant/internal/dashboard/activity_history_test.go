package dashboard_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// historyHandler builds the Dashboard over the real database and a ring, the
// same wiring main uses: the ring is the fast first page, the history answers
// everything older.
// historyRow is the subset of one request row these tests assert.
type historyRow struct {
	ID         string
	Transcript string
	Reply      string
	Source     string
	Outcome    string
	Summary    bool
}

func historyHandler(t *testing.T, ring *activity.Store) (*dashboard.Handler, *activity.History) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	history := activity.NewHistory(st.DB(), activity.HistoryOptions{})
	if ring == nil {
		ring = activity.NewWithHistory(activity.DefaultLimit, history)
	}
	return newHandler(t, dashboard.Options{Activity: ring}), history
}

func seed(t *testing.T, history *activity.History, entries ...activity.Entry) {
	t.Helper()
	for _, entry := range entries {
		if err := history.Add(entry); err != nil {
			t.Fatalf("history.Add: %v", err)
		}
	}
}

func observed(id string, at time.Time, source latency.Source) latency.Completed {
	return latency.Completed{
		ID:         id,
		Source:     source,
		Path:       "fast_path",
		Tool:       "calendar.get_events",
		Transcript: "what is on today",
		Outcome:    latency.OutcomeOK,
		Detail:     &latency.Detail{Reply: "nothing until four"},
	}
}

func TestActivityPagesThroughDurableHistoryAfterTheRingRollsOver(t *testing.T) {
	handler, history := historyHandler(t, nil)
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	// Older than any ring: the page must still read it after a restart.
	for i := 0; i < 3; i++ {
		seed(t, history, activity.Entry{
			ID: fmt.Sprintf("old-%d", i), At: base.Add(time.Duration(i) * time.Minute),
			Source: "voice", Path: "fast_path", Outcome: "ok",
			Transcript: "request from last week", Reply: "an old answer",
		})
	}

	var first struct {
		Requests   []historyRow
		HasHistory bool
		Next       string
	}
	rec := getJSON(t, handler, "/api/ui/activity", &first)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !first.HasHistory {
		t.Fatal("HasHistory = false with a durable store attached")
	}
	if len(first.Requests) != 3 {
		t.Fatalf("requests = %d, want the three durable rows", len(first.Requests))
	}
	if first.Requests[0].ID != "old-2" {
		t.Fatalf("first row = %q, want newest first", first.Requests[0].ID)
	}
	if first.Requests[0].Transcript != "request from last week" {
		t.Fatalf("row lost its text: %+v", first.Requests[0])
	}
}

func TestActivityFirstPageMergesTheRingWithoutDuplicatingRows(t *testing.T) {
	historyStore, err := store.Open(context.Background(), t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	history := activity.NewHistory(historyStore.DB(), activity.HistoryOptions{})
	ring := activity.NewWithHistory(activity.DefaultLimit, history)

	// One request that the ring observed and the history also holds.
	ring.Observe(observed("req-new", time.Now(), latency.SourceVoice))
	// One entry that only the durable store holds (for example written before
	// this process started).
	seed(t, history, activity.Entry{
		ID: "req-old", At: time.Now().Add(-48 * time.Hour), Source: "voice",
		Path: "fast_path", Outcome: "ok", Transcript: "yesterday", Reply: "old",
	})

	handler := newHandler(t, dashboard.Options{Activity: ring})
	var view struct {
		Requests []historyRow
		Next     string
	}
	getJSON(t, handler, "/api/ui/activity", &view)
	if len(view.Requests) != 2 {
		t.Fatalf("requests = %d, want the ring row and the durable row once each: %+v", len(view.Requests), view.Requests)
	}
	if view.Requests[0].ID != "req-new" || view.Requests[1].ID != "req-old" {
		t.Fatalf("order = %q, %q; want newest first", view.Requests[0].ID, view.Requests[1].ID)
	}
	if view.Next != "" {
		t.Fatalf("Next = %q with no older page", view.Next)
	}
}

func TestActivitySearchAndFiltersReadTheDurableHistory(t *testing.T) {
	handler, history := historyHandler(t, nil)
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	seed(t, history,
		activity.Entry{ID: "voice-hit", At: base, Source: "voice", Path: "fast_path", Outcome: "ok",
			Transcript: "set the blue timer", Reply: "done"},
		activity.Entry{ID: "typed-hit", At: base.Add(time.Minute), Source: "dashboard", Path: "fast_path", Outcome: "ok",
			Transcript: "typed the blue timer", Reply: "done"},
		activity.Entry{ID: "failed", At: base.Add(2 * time.Minute), Source: "voice", Path: "agent_path", Outcome: "failed",
			Transcript: "the weather failed", Error: "provider timed out"},
		activity.Entry{ID: "unrelated", At: base.Add(3 * time.Minute), Source: "voice", Path: "fast_path", Outcome: "ok",
			Transcript: "play music", Reply: "playing"},
	)

	var hit struct {
		Requests []historyRow
		Query    string
		Filter   string
	}
	getJSON(t, handler, "/api/ui/activity?q=blue", &hit)
	if len(hit.Requests) != 2 {
		t.Fatalf("search = %d rows, want the two blue requests", len(hit.Requests))
	}
	if hit.Query != "blue" {
		t.Errorf("Query = %q", hit.Query)
	}
	for _, r := range hit.Requests {
		if r.ID != "typed-hit" && r.ID != "voice-hit" {
			t.Errorf("unexpected search result %q", r.ID)
		}
	}

	var problems struct {
		Requests []historyRow
	}
	getJSON(t, handler, "/api/ui/activity?filter=problems", &problems)
	if len(problems.Requests) != 1 || problems.Requests[0].ID != "failed" {
		t.Fatalf("problems = %+v, want the one failed request", problems.Requests)
	}

	var typed struct {
		Requests []historyRow
	}
	getJSON(t, handler, "/api/ui/activity?filter=typed&q=blue", &typed)
	if len(typed.Requests) != 1 || typed.Requests[0].ID != "typed-hit" {
		t.Fatalf("typed search = %+v, want the keyboard request", typed.Requests)
	}

	// An unknown filter is All, not an error page.
	var unknown struct {
		Requests []historyRow
	}
	rec := getJSON(t, handler, "/api/ui/activity?filter=nonsense", &unknown)
	if rec.Code != 200 || len(unknown.Requests) != 4 {
		t.Fatalf("status = %d, rows = %d; want All", rec.Code, len(unknown.Requests))
	}
}

func TestActivityCursorPagesToTheOldestRow(t *testing.T) {
	handler, history := historyHandler(t, nil)
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 120; i++ {
		seed(t, history, activity.Entry{
			ID: fmt.Sprintf("req-%03d", i), At: base.Add(time.Duration(i) * time.Second),
			Source: "voice", Path: "fast_path", Outcome: "ok",
			Transcript: fmt.Sprintf("request %d", i),
		})
	}
	seen := map[string]bool{}
	before := ""
	pages := 0
	for {
		var page struct {
			Requests []historyRow
			Next     string
		}
		target := "/api/ui/activity"
		if before != "" {
			target += "?before=" + before
		}
		getJSON(t, handler, target, &page)
		pages++
		for _, r := range page.Requests {
			if seen[r.ID] {
				t.Fatalf("%q returned twice", r.ID)
			}
			seen[r.ID] = true
		}
		before = page.Next
		if before == "" {
			break
		}
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 120 {
		t.Fatalf("paged %d rows, want all 120", len(seen))
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 50+50+20", pages)
	}
}

func TestActivityHistoryRowsSurviveARingRestart(t *testing.T) {
	historyStore, err := store.Open(context.Background(), t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	history := activity.NewHistory(historyStore.DB(), activity.HistoryOptions{})
	before := activity.NewWithHistory(activity.DefaultLimit, history)
	before.Observe(observed("req-before-restart", time.Now().Add(-time.Hour), latency.SourceVoice))

	// A restart drops the ring but keeps the database.
	after := activity.NewWithHistory(activity.DefaultLimit, activity.NewHistory(historyStore.DB(), activity.HistoryOptions{}))
	handler := newHandler(t, dashboard.Options{Activity: after})
	var view struct {
		Requests   []historyRow
		HasHistory bool
	}
	getJSON(t, handler, "/api/ui/activity", &view)
	if len(view.Requests) != 1 || view.Requests[0].ID != "req-before-restart" {
		t.Fatalf("requests after restart = %+v, want the durable row", view.Requests)
	}
	if view.Requests[0].Transcript != "what is on today" {
		t.Fatalf("transcript after restart = %q", view.Requests[0].Transcript)
	}
}
