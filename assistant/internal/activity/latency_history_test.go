package activity_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
)

// TestHistoryLatency asserts that History.Latency computes percentiles correctly
// across windows, excludes failed/cancelled rows, and excludes unmeasured stages.
func TestHistoryLatency(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	history, _ := openHistory(t, activity.HistoryOptions{
		Now:              func() time.Time { return now },
		SkipSearchWarmup: true,
	})

	respMS := func(ms int64) *int64 { return &ms }

	// Seed fixture entries across time windows and outcomes.
	entries := []activity.Entry{
		{
			ID:         "req-1",
			At:         now.Add(-2 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "ok",
			ResponseMS: respMS(2000),
			Stages: []activity.Timing{
				{Name: "endpointing", MS: 500},
				{Name: "stt_finalize", MS: 150},
				{Name: "routing", MS: 40},
				{Name: "agent_first_token", MS: 1000},
				{Name: "tts_first_audio", MS: 120},
			},
			Checkpoints: []activity.Timing{
				{Name: "first_chunk", MS: 1500},
			},
		},
		{
			ID:         "req-2",
			At:         now.Add(-4 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "ok",
			ResponseMS: respMS(2400),
			Stages: []activity.Timing{
				{Name: "endpointing", MS: 550},
				{Name: "stt_finalize", MS: 200},
				{Name: "routing", MS: 60},
				{Name: "agent_first_token", MS: 1200},
				{Name: "tts_first_audio", MS: 180},
			},
			Checkpoints: []activity.Timing{
				{Name: "first_chunk", MS: 1800},
			},
		},
		{
			ID:         "req-3",
			At:         now.Add(-6 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "ok",
			ResponseMS: respMS(1800),
			Stages: []activity.Timing{
				{Name: "routing", MS: 50},
			},
			// No endpointing, stt_finalize, tts_first_audio, or first_chunk
		},
		{
			ID:         "req-failed",
			At:         now.Add(-1 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "failed",
			Error:      "internal error",
			ResponseMS: respMS(500),
			Stages: []activity.Timing{
				{Name: "routing", MS: 10},
			},
		},
		{
			ID:         "req-cancelled",
			At:         now.Add(-3 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "cancelled",
			ResponseMS: respMS(300),
			Stages: []activity.Timing{
				{Name: "routing", MS: 20},
			},
		},
		{
			ID:         "req-2d",
			At:         now.Add(-48 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "ok",
			ResponseMS: respMS(3000),
			Stages: []activity.Timing{
				{Name: "routing", MS: 80},
			},
		},
		{
			ID:         "req-10d",
			At:         now.Add(-10 * 24 * time.Hour),
			Source:     "voice",
			Path:       "agent_path",
			Outcome:    "ok",
			ResponseMS: respMS(4000),
			Stages: []activity.Timing{
				{Name: "routing", MS: 90},
			},
		},
	}

	for _, e := range entries {
		if err := history.Add(e); err != nil {
			t.Fatalf("history.Add(%s): %v", e.ID, err)
		}
	}

	// 24h window test
	summary24h, err := history.Latency(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("history.Latency(24h): %v", err)
	}

	// 5 rows within 24h
	if summary24h.Records != 5 {
		t.Fatalf("got %d records for 24h window, want 5", summary24h.Records)
	}

	if len(summary24h.Paths) != 1 {
		t.Fatalf("got %d paths, want 1", len(summary24h.Paths))
	}
	p := summary24h.Paths[0]
	if p.Path != "agent_path" {
		t.Errorf("got path %q, want agent_path", p.Path)
	}
	if p.Attempts != 5 {
		t.Errorf("got %d attempts, want 5", p.Attempts)
	}
	if p.Outcomes["ok"] != 3 || p.Outcomes["failed"] != 1 || p.Outcomes["cancelled"] != 1 {
		t.Errorf("unexpected outcomes: %v", p.Outcomes)
	}

	// Response percentile: only ok rows (1800, 2000, 2400)
	if p.Response.N != 3 {
		t.Errorf("got Response.N = %d, want 3", p.Response.N)
	}
	if p.Response.P50 != 2000 {
		t.Errorf("got Response.P50 = %v, want 2000", p.Response.P50)
	}
	if p.Response.P95 != 2400 {
		t.Errorf("got Response.P95 = %v, want 2400", p.Response.P95)
	}

	// Routing stage: present on 3 ok rows (40, 50, 60); failed (10) and cancelled (20) excluded
	routingStat := p.Stages["routing_ms"]
	if routingStat.N != 3 {
		t.Errorf("got routing N = %d, want 3", routingStat.N)
	}
	if routingStat.P50 != 50 {
		t.Errorf("got routing P50 = %v, want 50", routingStat.P50)
	}
	if routingStat.P95 != 60 {
		t.Errorf("got routing P95 = %v, want 60", routingStat.P95)
	}

	// TTS first audio stage: only present on 2 ok rows (120, 180); req-3 excluded, not 0
	ttsStat := p.Stages["tts_first_audio_ms"]
	if ttsStat.N != 2 {
		t.Errorf("got tts_first_audio N = %d, want 2 (missing measurement must not count as 0)", ttsStat.N)
	}
	if ttsStat.P50 != 120 {
		t.Errorf("got tts_first_audio P50 = %v, want 120", ttsStat.P50)
	}
	if ttsStat.P95 != 180 {
		t.Errorf("got tts_first_audio P95 = %v, want 180", ttsStat.P95)
	}

	// FirstChunk checkpoint: present on 2 ok rows (1500, 1800)
	if p.FirstChunk.N != 2 {
		t.Errorf("got FirstChunk.N = %d, want 2", p.FirstChunk.N)
	}
	if p.FirstChunk.P50 != 1500 {
		t.Errorf("got FirstChunk.P50 = %v, want 1500", p.FirstChunk.P50)
	}
	if p.FirstChunk.P95 != 1800 {
		t.Errorf("got FirstChunk.P95 = %v, want 1800", p.FirstChunk.P95)
	}

	// AgentToken: present on 2 ok rows (1000, 1200)
	if p.AgentToken.N != 2 {
		t.Errorf("got AgentToken.N = %d, want 2", p.AgentToken.N)
	}
	if p.AgentToken.P50 != 1000 {
		t.Errorf("got AgentToken.P50 = %v, want 1000", p.AgentToken.P50)
	}
	if p.AgentToken.P95 != 1200 {
		t.Errorf("got AgentToken.P95 = %v, want 1200", p.AgentToken.P95)
	}

	// 7d window test: should include req-2d (48h ago) but not req-10d (10d ago)
	summary7d, err := history.Latency(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("history.Latency(7d): %v", err)
	}
	if summary7d.Records != 6 {
		t.Fatalf("got %d records for 7d window, want 6", summary7d.Records)
	}
	p7d := summary7d.Paths[0]
	// Routing stage in 7d window has 4 ok rows: 40, 50, 60, 80
	routingStat7d := p7d.Stages["routing_ms"]
	if routingStat7d.N != 4 {
		t.Errorf("got 7d routing N = %d, want 4", routingStat7d.N)
	}
	if routingStat7d.P50 != 50 {
		t.Errorf("got 7d routing P50 = %v, want 50", routingStat7d.P50)
	}
	if routingStat7d.P95 != 80 {
		t.Errorf("got 7d routing P95 = %v, want 80", routingStat7d.P95)
	}
}
