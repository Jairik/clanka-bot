package latency

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestObserverSeesOneCompletedRequestWithDisplayDetail pins the one event the
// Dashboard Activity store consumes: route, timings, outcome and display-only
// Tool results, delivered exactly once at the same completion that logs.
func TestObserverSeesOneCompletedRequestWithDisplayDetail(t *testing.T) {
	var completed []Completed
	tl, now := newTestTimeline(t, SourceVoice)
	tl.observer = func(c Completed) { completed = append(completed, c) }
	ctx := WithContext(context.Background(), tl)
	tl.SetLastVoice(now.Add(-500 * time.Millisecond))

	SetRoute(ctx, "fast_path", "secret.read", "read my secret")
	SetStage(ctx, StageRouting, 40*time.Millisecond)
	AddToolCall(ctx, ToolCallDetail{
		Name:      "secret.read",
		LocalOnly: true,
		Result:    "the launch code is 1234",
	})
	SetSummary(ctx, "The launch code is 1234.", true, false)

	*now = now.Add(900 * time.Millisecond)
	MarkReplyAudio(ctx)
	SetStage(ctx, StageTTSFirstAudio, 120*time.Millisecond)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tl.Complete(logger, OutcomeOK, nil)
	// A second completion is ignored, so the observer cannot see one request
	// twice.
	tl.Complete(logger, OutcomeOK, nil)

	if len(completed) != 1 {
		t.Fatalf("observer calls = %d, want 1", len(completed))
	}
	got := completed[0]
	if got.ID != "req-1" || got.Source != SourceVoice || got.Path != "fast_path" {
		t.Fatalf("unexpected completion identity: %+v", got)
	}
	if got.Outcome != OutcomeOK || !got.HasResponse || got.Response != 1400*time.Millisecond {
		t.Fatalf("unexpected outcome/response: %+v", got)
	}
	if got.Stages[StageRouting] != 40*time.Millisecond {
		t.Fatalf("routing stage = %v, want 40ms", got.Stages[StageRouting])
	}
	if got.Detail == nil {
		t.Fatal("display detail missing")
	}
	if !got.Detail.LocalOnly || !strings.Contains(got.Detail.Reply, "launch code") {
		t.Fatalf("unexpected display detail: %+v", got.Detail)
	}
	if len(got.Detail.Calls) != 1 || !got.Detail.Calls[0].LocalOnly {
		t.Fatalf("tool call detail = %+v", got.Detail.Calls)
	}

	// The structured request line still carries route and timings, but not the
	// display-only reply or Local-only Tool result.
	line := logs.String()
	for _, unwanted := range []string{"launch code", "1234", "LocalOnly", "ToolCallDetail"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("request log leaked display detail %q:\n%s", unwanted, line)
		}
	}
	if !strings.Contains(line, `"path":"fast_path"`) || !strings.Contains(line, `"routing_ms":40`) {
		t.Fatalf("request log lost routing fields:\n%s", line)
	}
}

// TestObserverIsOptional keeps the many timelines created without an Activity
// page on the original path: Complete still logs and never panics.
func TestObserverIsOptional(t *testing.T) {
	tl, now := newTestTimeline(t, SourceDebug)
	_ = now
	var logs bytes.Buffer
	tl.Complete(slog.New(slog.NewJSONHandler(&logs, nil)), OutcomeNoAudio, nil)
	if !strings.Contains(logs.String(), `"outcome":"no_audio"`) {
		t.Fatalf("completion did not log: %s", logs.String())
	}
	if tl.DisplayDetail() != nil {
		t.Fatal("a request without routing produced display detail")
	}
}
