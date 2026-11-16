package activity_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

func TestObserveCarriesTokenCountsIntoTheEntry(t *testing.T) {
	store := activity.New(5)
	store.Observe(latency.Completed{
		ID: "req-tokens", Path: "agent_path", Transcript: "how many",
		Outcome:      latency.OutcomeOK,
		PromptTokens: 12000, CompletionTokens: 34,
		TotalTokens: 12034, CachedTokens: 512, Steps: 2,
	})
	entry, ok := store.Get("req-tokens")
	if !ok {
		t.Fatal("entry missing")
	}
	if entry.PromptTokens != 12000 || entry.CompletionTokens != 34 ||
		entry.TotalTokens != 12034 || entry.CachedTokens != 512 || entry.Steps != 2 {
		t.Fatalf("token counts lost on Observe: %+v", entry)
	}
}

// TestHistoryTokenCountsRoundTripAndPreMigrationRowReadsZero proves the counts
// survive the durable store and that a row written before migration 14 (whose
// new columns are NULL) reads as zero/unknown rather than failing or looking
// free.
func TestHistoryTokenCountsRoundTripAndPreMigrationRowReadsZero(t *testing.T) {
	ctx := context.Background()
	history, st := openHistory(t, activity.HistoryOptions{})
	base := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

	if err := history.Add(entry("with-tokens", base, func(e *activity.Entry) {
		e.Path = "agent_path"
		e.PromptTokens = 12000
		e.CompletionTokens = 34
		e.TotalTokens = 12034
		e.CachedTokens = 512
		e.Steps = 2
	})); err != nil {
		t.Fatalf("Add(with-tokens): %v", err)
	}

	// Reproduce a pre-migration row: insert only the original columns, leaving
	// the five token columns NULL the way an old database file has them.
	_, err := st.DB().ExecContext(ctx, `
		INSERT INTO activity (request_id, at, source, path, tool, outcome, error,
			transcript, reply, local_only, stages, checkpoints, calls, search, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"old-row", base.Add(time.Minute).UnixMilli(), "voice", "agent_path", "",
		"ok", "", "old question", "old answer", 0, "[]", "[]", "[]", "old question old answer", 0)
	if err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	page, err := history.QueryPage(ctx, activity.Query{Limit: 10})
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	byID := make(map[string]activity.Entry, len(page.Entries))
	for _, e := range page.Entries {
		byID[e.ID] = e
	}
	if len(byID) != 2 {
		t.Fatalf("entries = %d, want 2", len(byID))
	}

	got := byID["with-tokens"]
	if got.PromptTokens != 12000 || got.CompletionTokens != 34 ||
		got.TotalTokens != 12034 || got.CachedTokens != 512 || got.Steps != 2 {
		t.Fatalf("token counts did not round-trip: %+v", got)
	}
	old := byID["old-row"]
	if old.PromptTokens != 0 || old.CompletionTokens != 0 ||
		old.TotalTokens != 0 || old.CachedTokens != 0 || old.Steps != 0 {
		t.Fatalf("pre-migration row did not read zero: %+v", old)
	}
}
