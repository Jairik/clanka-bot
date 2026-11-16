package dashboard_test

import (
	"net/http"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

type tokenRow struct {
	ID               string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	AgentSteps       int
}

// TestActivityViewIncludesTokenCountsAndOldRowReadsZero proves the Dashboard
// view carries the per-turn counts and that a row without them (an old row, or
// a provider that omitted usage) reaches the page as zero, which the React
// component renders as "unknown" rather than "0" or "free".
func TestActivityViewIncludesTokenCountsAndOldRowReadsZero(t *testing.T) {
	f := newActivityFixture(t)
	f.store.Observe(latency.Completed{
		ID: "with-tokens", Path: "agent_path", Transcript: "how many tokens",
		Outcome: latency.OutcomeOK, Detail: &latency.Detail{Reply: "twelve thousand"},
		PromptTokens: 12000, CompletionTokens: 34,
		TotalTokens: 12034, CachedTokens: 512, Steps: 2,
	})
	// A stored row from before usage was recorded has no counts.
	f.store.Observe(latency.Completed{
		ID: "old-row", Path: "agent_path", Transcript: "an older question",
		Outcome: latency.OutcomeOK, Detail: &latency.Detail{Reply: "older answer"},
	})

	var view struct {
		Requests []tokenRow
	}
	rec := getJSON(f.t, f.handler, "/api/ui/activity", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET activity = %d: %s", rec.Code, rec.Body.String())
	}
	byID := make(map[string]tokenRow, len(view.Requests))
	for _, row := range view.Requests {
		byID[row.ID] = row
	}
	if len(byID) != 2 {
		t.Fatalf("view rows = %d, want 2: %+v", len(byID), view.Requests)
	}

	got := byID["with-tokens"]
	if got.PromptTokens != 12000 || got.CompletionTokens != 34 ||
		got.TotalTokens != 12034 || got.CachedTokens != 512 || got.AgentSteps != 2 {
		t.Fatalf("view token fields = %+v, want 12000/34/12034/512/2", got)
	}
	old := byID["old-row"]
	if old.PromptTokens != 0 || old.CompletionTokens != 0 ||
		old.TotalTokens != 0 || old.CachedTokens != 0 || old.AgentSteps != 0 {
		t.Fatalf("old row token fields = %+v, want all zero", old)
	}
}
