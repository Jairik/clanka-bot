package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

func ptr(v float64) *float64 { return &v }

// TestWriteFlagsOverStageButKeepsOverallAndNoBudget verifies the four CLI
// behaviours from #218: a stage over its p95 prints "OVER routing p95" while the
// overall Response stays within budget, a stage with no budget prints no flag,
// and a path with no budget still prints "(no budget)" for its overall row.
func TestWriteFlagsOverStageButKeepsOverallAndNoBudget(t *testing.T) {
	records := []latency.Record{
		// Three Fast path turns: routing p50 40 / p95 200 trips the stage p95,
		// every other stage and the overall Response stay within budget.
		{Path: "fast_path", Outcome: "ok", ResponseMS: ptr(900), Stages: map[string]int64{
			"routing_ms": 40, "tool_execute_ms": 50, "endpointing_ms": 400, "stt_finalize_ms": 100, "tts_first_audio_ms": 100,
		}},
		{Path: "fast_path", Outcome: "ok", ResponseMS: ptr(900), Stages: map[string]int64{
			"routing_ms": 40, "tool_execute_ms": 50, "endpointing_ms": 400, "stt_finalize_ms": 100, "tts_first_audio_ms": 100,
		}},
		{Path: "fast_path", Outcome: "ok", ResponseMS: ptr(900), Stages: map[string]int64{
			"routing_ms": 200, "tool_execute_ms": 50, "endpointing_ms": 400, "stt_finalize_ms": 100, "tts_first_audio_ms": 100,
		}},
		// A path with no budget at all: its overall row must print "(no budget)".
		{Path: "mystery_path", Outcome: "ok", ResponseMS: ptr(500), Stages: map[string]int64{"routing_ms": 5}},
	}

	summary := latency.Summarize(records)
	var buf bytes.Buffer
	write(&buf, summary, "voice")
	out := buf.String()

	if !strings.Contains(out, "OVER routing p95") {
		t.Errorf("expected an OVER routing p95 flag, got:\n%s", out)
	}
	if strings.Contains(out, "OVER tool_execute") {
		t.Errorf("tool_execute has no budget and must not be flagged:\n%s", out)
	}
	if !strings.Contains(out, "(no budget)") {
		t.Errorf("a path with no budget must still print (no budget):\n%s", out)
	}
	if !strings.Contains(out, "within budget") {
		t.Errorf("the Fast path overall Response must stay within budget:\n%s", out)
	}
}
