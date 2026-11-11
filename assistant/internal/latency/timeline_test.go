package latency

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newTestTimeline returns a timeline with a controllable clock, so stage and
// checkpoint values are exact rather than timing-dependent.
func newTestTimeline(t *testing.T, source Source) (*Timeline, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tl := New("req-1", source)
	tl.now = func() time.Time { return now }
	tl.created = now
	return tl, &now
}

func TestTimelineRecordsStageDurationsAndFirstWins(t *testing.T) {
	tl, _ := newTestTimeline(t, SourceVoice)
	ctx := WithContext(context.Background(), tl)

	SetStage(ctx, StageRouting, 40*time.Millisecond)
	SetStage(ctx, StageRouting, 900*time.Millisecond) // first mark is kept
	AddStage(ctx, StageToolExecute, 30*time.Millisecond)
	AddStage(ctx, StageToolExecute, 20*time.Millisecond)

	if got, _ := tl.Duration(StageRouting); got != 40*time.Millisecond {
		t.Fatalf("routing = %v, want 40ms", got)
	}
	if got, _ := tl.Duration(StageToolExecute); got != 50*time.Millisecond {
		t.Fatalf("tool execute = %v, want 50ms (accumulated)", got)
	}
	if got := tl.Count(StageToolExecute); got != 2 {
		t.Fatalf("tool execute count = %d, want 2", got)
	}
}

func TestTimelineResponseLatencyRunsFromLastVoicedFrameToFirstReplyAudio(t *testing.T) {
	tl, now := newTestTimeline(t, SourceVoice)

	// The user's last voiced frame is 500 ms before the timeline opens.
	lastVoice := now.Add(-500 * time.Millisecond)
	tl.SetLastVoice(lastVoice)

	*now = now.Add(1000 * time.Millisecond)
	MarkReplyAudio(WithContext(context.Background(), tl))

	got, ok := tl.Response()
	if !ok {
		t.Fatal("response latency not measured")
	}
	if got != 1500*time.Millisecond {
		t.Fatalf("response latency = %v, want 1.5s", got)
	}

	// A chime or filler plays earlier and does not move the reply endpoint.
	*now = now.Add(2 * time.Second)
	MarkReplyAudio(WithContext(context.Background(), tl))
	if got, _ := tl.Response(); got != 1500*time.Millisecond {
		t.Fatalf("response latency moved after a second mark: %v", got)
	}
}

func TestTimelineWithoutVoiceStartHasNoResponseLatency(t *testing.T) {
	tl, now := newTestTimeline(t, SourceDebug)
	*now = now.Add(time.Second)
	MarkReplyAudio(WithContext(context.Background(), tl))

	if _, ok := tl.Response(); ok {
		t.Fatal("typed debug request must not report a spoken response latency")
	}
}

func TestTimelineCheckpointIsTimeSinceTurnStart(t *testing.T) {
	tl, now := newTestTimeline(t, SourceVoice)
	tl.SetLastVoice(now.Add(-200 * time.Millisecond))

	*now = now.Add(800 * time.Millisecond)
	Checkpoint(WithContext(context.Background(), tl), CheckpointFirstChunk)

	if got, ok := tl.Checkpoint(CheckpointFirstChunk); !ok || got != 1000*time.Millisecond {
		t.Fatalf("first chunk checkpoint = %v (ok=%v), want 1s", got, ok)
	}
}

func TestCompleteLogsMissingStagesAndOutcomeWithoutZeroFilling(t *testing.T) {
	tl, now := newTestTimeline(t, SourceVoice)
	tl.SetLastVoice(now.Add(-500 * time.Millisecond))
	SetRoute(WithContext(context.Background(), tl), "fast_path", "time.now", "what time is it")
	SetStage(WithContext(context.Background(), tl), StageRouting, 40*time.Millisecond)

	*now = now.Add(900 * time.Millisecond)
	MarkReplyAudio(WithContext(context.Background(), tl))
	SetStage(WithContext(context.Background(), tl), StageTTSFirstAudio, 120*time.Millisecond)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tl.Complete(logger, OutcomeOK, nil)

	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, logs.String())
	}
	if line["msg"] != "request" || line["request_id"] != "req-1" {
		t.Fatalf("unexpected log line: %v", line)
	}
	if line["outcome"] != "ok" || line["path"] != "fast_path" {
		t.Fatalf("unexpected outcome/path: %v", line)
	}
	if line["response_ms"] != float64(1400) {
		t.Fatalf("response_ms = %v, want 1400", line["response_ms"])
	}

	stages, ok := line["stages"].(map[string]any)
	if !ok {
		t.Fatalf("stages missing: %v", line)
	}
	if stages["routing_ms"] != float64(40) {
		t.Fatalf("routing_ms = %v, want 40", stages["routing_ms"])
	}
	// Missing stages are absent, never a fabricated zero.
	if _, exists := stages["endpointing_ms"]; exists {
		t.Fatalf("endpointing_ms fabricated: %v", stages)
	}
	if _, exists := stages["agent_first_token_ms"]; exists {
		t.Fatalf("agent_first_token_ms fabricated: %v", stages)
	}
	if stages["tts_first_audio_ms"] != float64(120) {
		t.Fatalf("tts_first_audio_ms = %v, want 120", stages["tts_first_audio_ms"])
	}
}

func TestCompleteLogsCancelledWithoutResponse(t *testing.T) {
	tl, _ := newTestTimeline(t, SourceVoice)
	tl.SetLastVoice(time.Now().Add(-2 * time.Second))

	var logs bytes.Buffer
	tl.Complete(slog.New(slog.NewJSONHandler(&logs, nil)), OutcomeCancelled, context.Canceled)

	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if line["outcome"] != "cancelled" {
		t.Fatalf("outcome = %v, want cancelled", line["outcome"])
	}
	if _, exists := line["response_ms"]; exists {
		t.Fatalf("cancelled request reported a response latency: %v", line)
	}
	if !strings.Contains(line["error"].(string), "canceled") {
		t.Fatalf("error = %v, want the cancellation", line["error"])
	}
}

func TestCompleteLogsOnce(t *testing.T) {
	tl, _ := newTestTimeline(t, SourceVoice)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	tl.Complete(logger, OutcomeNoAudio, nil)
	tl.Complete(logger, OutcomeNoAudio, nil)

	if n := strings.Count(logs.String(), `"msg":"request"`); n != 1 {
		t.Fatalf("request lines = %d, want 1\n%s", n, logs.String())
	}
}

func TestCompletedTimelineIgnoresLateStages(t *testing.T) {
	tl, _ := newTestTimeline(t, SourceVoice)
	tl.Complete(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), OutcomeNoAudio, nil)
	SetStage(WithContext(context.Background(), tl), StageRouting, time.Second)

	if _, ok := tl.Duration(StageRouting); ok {
		t.Fatal("a completed timeline accepted a late stage mark")
	}
}

func TestFromContextWithoutTimelineIsSafe(t *testing.T) {
	ctx := context.Background()
	if FromContext(ctx) != nil {
		t.Fatal("FromContext returned a timeline for a plain context")
	}
	// Every helper must no-op without a timeline.
	SetStage(ctx, StageRouting, time.Second)
	AddStage(ctx, StageToolExecute, time.Second)
	MarkReplyAudio(ctx)
	Checkpoint(ctx, CheckpointFirstChunk)
	SetRoute(ctx, "fast_path", "time.now", "hi")
	SetAttr(ctx, "k", "v")
	SetFailure(ctx, "boom")
	tl := New("req-2", SourceDebug)
	tl.now = time.Now
	Complete(ctx, slog.Default(), OutcomeOK, nil)
}
