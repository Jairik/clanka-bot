package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// routingBudget is the ceiling for a route whose only cost is the decision.
// Both regressions this file covers put a 100-150 ms action inside routing_ms.
const routingBudget = 20 * time.Millisecond

// TestFocusIntentActionIsNotRouting covers #192's first symptom: a holder that
// takes 100 ms to pause music must report that time as tool_execute, and the
// routing stage must stay at decision cost.
func TestFocusIntentActionIsNotRouting(t *testing.T) {
	var registry focus.Registry
	holder := &focusHolder{name: "Paused", intents: []focus.Intent{focus.Pause}, delay: 100 * time.Millisecond}
	claim := registry.Claim(focus.Reply, holder)
	defer claim.Release()

	p, _ := newFocusPipeline(t, map[string]string{"pause": "pause"}, &registry)
	timeline := latency.New("req-focus-intent", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)

	reply := p.Handle(ctx, request(), "pause")

	// Behaviour is unchanged: the holder's line is still the reply.
	if reply.Path != tool.PathFocusIntent {
		t.Fatalf("path = %q, want %q", reply.Path, tool.PathFocusIntent)
	}
	if reply.Text != "Paused pause." {
		t.Fatalf("reply = %q, want the holder's line", reply.Text)
	}

	routing, ok := timeline.Duration(latency.StageRouting)
	if !ok {
		t.Fatal("routing stage missing from the timeline")
	}
	if routing >= routingBudget {
		t.Fatalf("routing = %v, want < %v: the holder's action was counted as routing", routing, routingBudget)
	}
	if count := timeline.Count(latency.StageRouting); count != 1 {
		t.Fatalf("routing marks = %d, want exactly one", count)
	}
	executed, ok := timeline.Duration(latency.StageToolExecute)
	if !ok {
		t.Fatal("tool execute stage missing: the holder's action was not attributed")
	}
	if executed < 90*time.Millisecond {
		t.Fatalf("tool execute = %v, want about the holder's 100 ms", executed)
	}
}

// slowEmbedder spends real time in the semantic step so a test can tell a
// routing stage that ran to the end from one frozen at an earlier match.
type slowEmbedder struct {
	delay time.Duration
	inner pipeline.Embedder
}

func (e slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	time.Sleep(e.delay)
	return e.inner.Embed(ctx, texts)
}

// TestFocusIntentFallThroughTimesTheWholeDecision keeps the other half honest:
// an intent the holder declines falls through to the semantic Fast path, and
// the exclusion must not stop routing at the declined offer.
func TestFocusIntentFallThroughTimesTheWholeDecision(t *testing.T) {
	var registry focus.Registry
	holder := &focusHolder{name: "Busy", intents: []focus.Intent{focus.Pause}, decline: true, delay: 30 * time.Millisecond}
	claim := registry.Claim(focus.Music, holder)
	defer claim.Release()

	router := pipeline.NewSemanticRouter(clauseRegistry(t), slowEmbedder{delay: 40 * time.Millisecond, inner: keywordEmbedder{}}, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	logger, _ := testLogger()
	p, err := pipeline.New(tool.NewRegistry(time.Second), map[string]string{"pause": "pause"}, logger,
		pipeline.WithSemanticRouter(router), pipeline.WithFocus(&registry))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	timeline := latency.New("req-focus-fallthrough", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)

	reply := p.Handle(ctx, request(), "pause")

	if reply.Path == tool.PathFocusIntent {
		t.Fatalf("path = %q, want a fall-through", reply.Path)
	}
	routing, ok := timeline.Duration(latency.StageRouting)
	if !ok {
		t.Fatal("routing stage missing after a fall-through")
	}
	if routing < 40*time.Millisecond {
		t.Fatalf("routing = %v, want the semantic step after the declined offer included", routing)
	}
	if count := timeline.Count(latency.StageRouting); count != 1 {
		t.Fatalf("routing marks = %d, want exactly one", count)
	}
	executed, ok := timeline.Duration(latency.StageToolExecute)
	if !ok || executed < 25*time.Millisecond {
		t.Fatalf("tool execute = %v/%v, want about the holder's 30 ms offer", executed, ok)
	}
}

// TestDeferredAgentFallbackKeepsStreamingAndRouting covers #192's second and
// third symptoms together: a deferred request with no safe action falls back to
// the Agent path, which must not be inside routing_ms, must keep the Agent-path
// Filler signal, and must forward the model's deltas to the reply session.
func TestDeferredAgentFallbackKeepsStreamingAndRouting(t *testing.T) {
	logger, logs := testLogger()
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Here is a joke."}
	runner.delay = 150 * time.Millisecond
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger, pipeline.WithAgent(runner, nil))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	queue := &pipeline.DeferredQueue{}
	p.BindDeferred(queue)

	timeline := latency.New("req-deferred-agent", latency.SourceVoice)
	ctx := latency.WithContext(context.Background(), timeline)

	agentPath := 0
	var deltas []string
	reply := p.HandleEvents(ctx, request(), "when the music stops, tell me a joke", pipeline.Events{
		AgentPath: func() { agentPath++ },
		Delta:     func(delta string) { deltas = append(deltas, delta) },
	})

	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want %q", reply.Path, tool.PathAgent)
	}
	if reply.Text != "Here is a joke." {
		t.Fatalf("reply = %q, want the Agent answer", reply.Text)
	}
	if calls := runner.calls(); len(calls) != 1 || calls[0] != "when the music stops, tell me a joke" {
		t.Fatalf("agent saw %v, want the whole transcript", calls)
	}
	if runner.nilDelta {
		t.Fatal("Agent ran without a delta callback: streaming was dropped")
	}
	if agentPath != 1 {
		t.Fatalf("Agent-path signals = %d, want exactly one Filler", agentPath)
	}
	if len(deltas) != 1 || deltas[0] != "partial" {
		t.Fatalf("deltas = %v, want [partial]", deltas)
	}
	if queued := queue.List(); len(queued) != 0 {
		t.Fatalf("queued %v, want nothing queued", queued)
	}

	routing, ok := timeline.Duration(latency.StageRouting)
	if !ok {
		t.Fatal("routing stage missing from the timeline")
	}
	if routing >= routingBudget {
		t.Fatalf("routing = %v, want < %v: the model call was counted as routing", routing, routingBudget)
	}

	// The pipeline must not write a second request line when a timeline owns
	// the request; the session's one line keeps the same path.
	if strings.Contains(logs.String(), `"msg":"request"`) {
		t.Fatalf("pipeline wrote a request line of its own:\n%s", logs.String())
	}
	line := completeLine(t, timeline, latency.OutcomeNoAudio)
	if line["path"] != string(tool.PathAgent) {
		t.Fatalf("path = %v, want %q", line["path"], tool.PathAgent)
	}
	if reason, exists := line["fallthrough_reason"]; exists {
		t.Fatalf("fallthrough_reason = %v, want absent on a handled route", reason)
	}
}

// TestQueuedDeferredReplyLogsNoFallthroughReason covers the third acceptance
// item: a queued reply is handled, so its request line carries no reason at all
// rather than "semantic_disabled".
func TestQueuedDeferredReplyLogsNoFallthroughReason(t *testing.T) {
	logger, logs := testLogger()
	p, err := pipeline.New(clauseRegistry(t), map[string]string{"set a timer for five minutes": "timer.set"}, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	queue := &pipeline.DeferredQueue{}
	p.BindDeferred(queue)

	reply := p.Handle(context.Background(), request(), "after the timer set a timer for five minutes")
	if reply.Path != tool.PathControlPhrase {
		t.Fatalf("path = %q, want %q", reply.Path, tool.PathControlPhrase)
	}
	if queued := queue.List(); len(queued) != 1 || queued[0].Event != "timer" {
		t.Fatalf("queued = %+v, want one timer action", queued)
	}

	record := logRecord(t, logs)
	if record["path"] != string(tool.PathControlPhrase) {
		t.Fatalf("path = %v, want %q", record["path"], tool.PathControlPhrase)
	}
	if reason, exists := record["fallthrough_reason"]; exists {
		t.Fatalf("fallthrough_reason = %v, want absent on a queued reply", reason)
	}
}

// completeLine renders the timeline's one request line without a reply audio
// mark, which is what a text-only or session-ended turn looks like.
func completeLine(t *testing.T, timeline *latency.Timeline, outcome latency.Outcome) map[string]any {
	t.Helper()
	var rendered bytes.Buffer
	timeline.Complete(slog.New(slog.NewJSONHandler(&rendered, nil)), outcome, nil)
	var line map[string]any
	if err := json.Unmarshal(rendered.Bytes(), &line); err != nil {
		t.Fatalf("request line is not JSON: %v\n%s", err, rendered.String())
	}
	return line
}
