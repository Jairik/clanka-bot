package dashboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// latencyViewShape matches the JSON response of /api/ui/latency.
type latencyViewShape struct {
	Window  string `json:"window"`
	Records int    `json:"records"`
	Paths   []struct {
		Path          string `json:"path"`
		Label         string `json:"label"`
		Attempts      int    `json:"attempts"`
		ResponseCount int    `json:"response_count"`
		Response      struct {
			N   int     `json:"n"`
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
		} `json:"response"`
		Stages map[string]struct {
			N   int     `json:"n"`
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
		} `json:"stages"`
		StageList []struct {
			Name  string  `json:"name"`
			Label string  `json:"label"`
			N     int     `json:"n"`
			P50   float64 `json:"p50"`
			P95   float64 `json:"p95"`
		} `json:"stage_list"`
		FirstChunk struct {
			N   int     `json:"n"`
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
		} `json:"first_chunk"`
		AgentToken struct {
			N   int     `json:"n"`
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
		} `json:"agent_token"`
	} `json:"paths"`
	Notice string `json:"notice"`
}

// TestLatencyRequiresIdentity asserts that /api/ui/latency enforces authentication.
func TestLatencyRequiresIdentity(t *testing.T) {
	handler, _ := historyHandler(t, nil)

	// An unauthenticated request must be rejected with 403.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, "/api/ui/latency", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated request code = %d, want 403", rec.Code)
	}

	// An authenticated request must succeed with 200.
	recAuth := httptest.NewRecorder()
	handler.ServeHTTP(recAuth, request(http.MethodGet, "/api/ui/latency", owner))
	if recAuth.Code != http.StatusOK {
		t.Fatalf("authenticated request code = %d, want 200", recAuth.Code)
	}
}

// TestLatencyWindowAndSummaries asserts that /api/ui/latency returns window-filtered percentiles.
func TestLatencyWindowAndSummaries(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/data/assistant.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	history := activity.NewHistory(st.DB(), activity.HistoryOptions{
		Now:              func() time.Time { return now },
		SkipSearchWarmup: true,
	})
	ring := activity.NewWithHistory(activity.DefaultLimit, history)
	handler := newHandler(t, dashboard.Options{Activity: ring})

	respMS := func(ms int64) *int64 { return &ms }

	// Seed one OK request within 24h, one failed within 24h, and one OK request from 3 days ago.
	seed(t, history,
		activity.Entry{
			ID:         "req-24h",
			At:         now.Add(-2 * time.Hour),
			Source:     "voice",
			Path:       "fast_path",
			Outcome:    "ok",
			ResponseMS: respMS(1200),
			Stages: []activity.Timing{
				{Name: "routing", MS: 35},
				{Name: "tts_first_audio", MS: 140},
			},
			Checkpoints: []activity.Timing{
				{Name: "first_chunk", MS: 950},
			},
		},
		activity.Entry{
			ID:         "req-failed",
			At:         now.Add(-1 * time.Hour),
			Source:     "voice",
			Path:       "fast_path",
			Outcome:    "failed",
			Error:      "failed turn",
			ResponseMS: respMS(400),
			Stages: []activity.Timing{
				{Name: "routing", MS: 10},
			},
		},
		activity.Entry{
			ID:         "req-3d",
			At:         now.Add(-3 * 24 * time.Hour),
			Source:     "voice",
			Path:       "fast_path",
			Outcome:    "ok",
			ResponseMS: respMS(1800),
			Stages: []activity.Timing{
				{Name: "routing", MS: 45},
				{Name: "tts_first_audio", MS: 200},
			},
			Checkpoints: []activity.Timing{
				{Name: "first_chunk", MS: 1400},
			},
		},
	)

	// Default window: 24h
	var view24h latencyViewShape
	rec := getJSON(t, handler, "/api/ui/latency", &view24h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ui/latency code = %d, want 200", rec.Code)
	}
	if view24h.Window != "24h" {
		t.Errorf("default window = %q, want 24h", view24h.Window)
	}
	if view24h.Records != 2 {
		t.Fatalf("24h records = %d, want 2", view24h.Records)
	}
	if len(view24h.Paths) != 1 {
		t.Fatalf("24h paths count = %d, want 1", len(view24h.Paths))
	}
	path24h := view24h.Paths[0]
	if path24h.Path != "fast_path" {
		t.Errorf("path = %q, want fast_path", path24h.Path)
	}
	if path24h.Response.N != 1 || path24h.Response.P50 != 1200 {
		t.Errorf("response stat = %+v, want N=1, P50=1200", path24h.Response)
	}
	if path24h.Stages["routing"].N != 1 || path24h.Stages["routing"].P50 != 35 {
		t.Errorf("routing stage = %+v, want N=1, P50=35", path24h.Stages["routing"])
	}
	if path24h.FirstChunk.N != 1 || path24h.FirstChunk.P50 != 950 {
		t.Errorf("first_chunk = %+v, want N=1, P50=950", path24h.FirstChunk)
	}

	// 7d window includes the 3-day old request
	var view7d latencyViewShape
	rec7d := getJSON(t, handler, "/api/ui/latency?window=7d", &view7d)
	if rec7d.Code != http.StatusOK {
		t.Fatalf("GET /api/ui/latency?window=7d code = %d, want 200", rec7d.Code)
	}
	if view7d.Window != "7d" {
		t.Errorf("window = %q, want 7d", view7d.Window)
	}
	if view7d.Records != 3 {
		t.Fatalf("7d records = %d, want 3", view7d.Records)
	}
	path7d := view7d.Paths[0]
	if path7d.Response.N != 2 {
		t.Errorf("7d Response.N = %d, want 2", path7d.Response.N)
	}
}
