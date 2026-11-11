package latency_test

import (
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

func TestPercentileUsesNearestRank(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	tests := []struct {
		p    float64
		want float64
	}{
		{p: 50, want: 50},
		{p: 95, want: 100},
		{p: 99, want: 100},
		{p: 10, want: 10},
	}
	for _, test := range tests {
		if got := latency.Percentile(values, test.p); got != test.want {
			t.Errorf("Percentile(p%v) = %v, want %v", test.p, got, test.want)
		}
	}
}

func TestReadRecordsKeepsOnlyRequestLines(t *testing.T) {
	logs := strings.Join([]string{
		`{"time":"2026-11-11T12:00:00Z","level":"INFO","msg":"speech","request_id":"a"}`,
		`{"time":"2026-11-11T12:00:01Z","level":"INFO","msg":"request","request_id":"a","source":"voice","path":"fast_path","outcome":"ok","response_ms":900,"stages":{"routing_ms":40},"checkpoints":{"first_chunk_ms":800}}`,
		`assistant-1  | {"msg":"request","request_id":"b","source":"debug","path":"agent_path","outcome":"ok","response_ms":2000}`,
		`not json at all`,
		`{"msg":"request","request_id":"c","source":"voice","path":"agent_path","outcome":"cancelled"}`,
	}, "\n")

	records, err := latency.ReadRecords(strings.NewReader(logs), "voice")
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2: %+v", len(records), records)
	}
	if records[0].RequestID != "a" || records[1].RequestID != "c" {
		t.Fatalf("unexpected records: %+v", records)
	}
	if records[0].ResponseMS == nil || *records[0].ResponseMS != 900 {
		t.Fatalf("response_ms not parsed: %+v", records[0])
	}
	if records[1].ResponseMS != nil {
		t.Fatalf("cancelled request invented a response latency: %+v", records[1])
	}
}

func response(value float64) *float64 { return &value }

func TestSummarizeNeverCountsFailedOrCancelledAsLatency(t *testing.T) {
	records := []latency.Record{
		{Path: "fast_path", Outcome: "ok", ResponseMS: response(900), Stages: map[string]int64{"routing_ms": 40, "tts_first_audio_ms": 100}},
		{Path: "fast_path", Outcome: "ok", ResponseMS: response(1400), Stages: map[string]int64{"routing_ms": 60, "tts_first_audio_ms": 200}},
		{Path: "fast_path", Outcome: "cancelled"},
		{Path: "fast_path", Outcome: "failed"},
		{Path: "agent_path", Outcome: "ok", ResponseMS: response(3000), Stages: map[string]int64{"agent_first_token_ms": 700}, Checkpoints: map[string]int64{"first_chunk_ms": 1100}},
	}

	summary := latency.Summarize(records)
	if summary.Records != 5 {
		t.Fatalf("records = %d, want 5", summary.Records)
	}

	fast := summary.Paths[0]
	for _, p := range summary.Paths {
		if p.Path == "fast_path" {
			fast = p
		}
	}
	if fast.Path != "fast_path" {
		t.Fatalf("fast path missing from %+v", summary.Paths)
	}
	if fast.Attempts != 4 || fast.Outcomes["ok"] != 2 || fast.Outcomes["cancelled"] != 1 || fast.Outcomes["failed"] != 1 {
		t.Fatalf("outcomes = %+v", fast.Outcomes)
	}
	if fast.Response.N != 2 || fast.Response.P50 != 900 || fast.Response.P95 != 1400 {
		t.Fatalf("response stat = %+v", fast.Response)
	}
	if _, exists := fast.Stages["agent_first_token_ms"]; exists {
		t.Fatal("fast path reported an agent token stage")
	}
	if fast.Stages["routing_ms"].P50 != 40 {
		t.Fatalf("routing stat = %+v", fast.Stages["routing_ms"])
	}

	agent := summary.Paths[len(summary.Paths)-1]
	for _, p := range summary.Paths {
		if p.Path == "agent_path" {
			agent = p
		}
	}
	if agent.AgentToken.P50 != 700 {
		t.Fatalf("agent token stat = %+v", agent.AgentToken)
	}
	if agent.FirstChunk.P50 != 1100 {
		t.Fatalf("first chunk stat = %+v", agent.FirstChunk)
	}
}

func TestOverBudgetOnlyWhenSamplesExist(t *testing.T) {
	empty := latency.PathSummary{Path: "fast_path"}
	if overP50, overP95 := empty.Over(latency.BudgetFast); overP50 || overP95 {
		t.Fatal("a path with no samples was flagged over budget")
	}

	slow := latency.PathSummary{
		ResponseCount: 3,
		Response:      latency.Stat{N: 3, P50: 1500, P95: 2600},
	}
	overP50, overP95 := slow.Over(latency.BudgetFast)
	if !overP50 || !overP95 {
		t.Fatalf("over flags = %v/%v, want both", overP50, overP95)
	}
}

func TestBudgetForRoutesControlAndConfirmationToFastPath(t *testing.T) {
	for _, path := range []string{"fast_path", "control_phrase", "confirmation"} {
		budget, ok := latency.BudgetFor(path)
		if !ok || budget != latency.BudgetFast {
			t.Errorf("BudgetFor(%q) = %+v, %v", path, budget, ok)
		}
	}
	if budget, ok := latency.BudgetFor("agent_path"); !ok || budget != latency.BudgetAgent {
		t.Errorf("agent budget = %+v, %v", budget, ok)
	}
	if _, ok := latency.BudgetFor("(unrouted)"); ok {
		t.Error("unrouted requests got a budget")
	}
}
