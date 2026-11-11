package pipeline_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// testRouteOut is the reply shape for the path-recording Tool.
type testRouteOut struct {
	Value string `json:"value"`
}

// TestSemanticFastPathThenAgentFallbackShareHistoryAndPrivacy checks that a
// semantic Fast path turn reaches the shared history marked Local-only, never
// appears in the outgoing prompt, and that a later open question reaches the
// Agent path with the full transcript.
func TestSemanticFastPathThenAgentFallbackShareHistoryAndPrivacy(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	local := tool.Tool[fakeIn, fakeOut]{
		Name:        "mail.inbox",
		Description: "Local-only inbox.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, fakeIn) (fakeOut, error) {
			return fakeOut{Value: "you have one message"}, nil
		},
		Speak:    func(out fakeOut) string { return out.Value },
		Examples: []string{"read my inbox"},
	}
	if err := tool.Add(reg, local); err != nil {
		t.Fatalf("Add: %v", err)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"read my inbox":       {1, 0},
		"open my inbox":       {1, 0},
		"why is the sky blue": {0, 1},
	}}
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "Because of scattering."}
	logger, logs := testLogger()
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgent(runner, []string{"agent"}),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "open my inbox")
	if reply.Path != tool.PathFast || reply.Tool != "mail.inbox" {
		t.Fatalf("reply = %+v, want the semantic Fast path", reply)
	}
	if calls := runner.calls(); len(calls) != 0 {
		t.Fatalf("agent was called %v, want the Fast path", calls)
	}

	turns := runner.History().Recent()
	if len(turns) != 1 || len(turns[0].Calls) != 1 {
		t.Fatalf("history = %+v, want one semantic Fast path turn", turns)
	}
	if !turns[0].Calls[0].LocalOnly {
		t.Fatal("semantic Fast path did not mark the Local-only call")
	}

	// Privacy: the Local-only turn must not reach the outgoing prompt.
	prompt, err := json.Marshal(agent.NewPromptBuilder("").Messages("why is the sky blue", turns))
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	for _, secret := range []string{"open my inbox", "you have one message", "mail.inbox"} {
		if strings.Contains(string(prompt), secret) {
			t.Fatalf("outgoing prompt leaked %q: %s", secret, prompt)
		}
	}

	reply = p.Handle(context.Background(), request(), "why is the sky blue")
	if reply.Path != tool.PathAgent || reply.Text != "Because of scattering." {
		t.Fatalf("reply = %+v, want the Agent path", reply)
	}
	if calls := runner.calls(); len(calls) != 1 || calls[0] != "why is the sky blue" {
		t.Fatalf("agent saw %v, want the full transcript", calls)
	}
	if !strings.Contains(logs.String(), `"fallthrough_reason":"below_threshold"`) {
		t.Errorf("missing fall-through reason in log: %s", logs.String())
	}
}

// TestSemanticFastPathCarriesTheFastPathRequest checks the Tool request path
// that #17 and downstream Tool handlers see for a semantic match.
func TestSemanticFastPathCarriesTheFastPathRequest(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	var seen tool.Request
	route := tool.Tool[fakeIn, testRouteOut]{
		Name:        "test.route",
		Description: "Records the request path.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ fakeIn) (testRouteOut, error) {
			req, _ := tool.RequestFrom(ctx)
			seen = req
			return testRouteOut{Value: "checked"}, nil
		},
		Speak:    func(out testRouteOut) string { return out.Value },
		Examples: []string{"check the route"},
	}
	if err := tool.Add(reg, route); err != nil {
		t.Fatalf("Add: %v", err)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"check the route":         {1, 0},
		"can you check the route": {1, 0},
	}}
	logger, _ := testLogger()
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	p, err := pipeline.New(reg, nil, logger, pipeline.WithSemanticRouter(router))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "can you check the route")
	if reply.Path != tool.PathFast || reply.Text != "checked" {
		t.Fatalf("reply = %+v, want the semantic Fast path", reply)
	}
	if seen.Path != tool.PathFast || seen.ID != "req-1" {
		t.Fatalf("tool request = %+v, want path fast_path and request id", seen)
	}
}

// TestEmbedFailureKeepsControlPhraseAndAgentWorking checks the degradation
// path when the embed service is down at request time.
func TestEmbedFailureKeepsControlPhraseAndAgentWorking(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it":  {1, 0},
		"what's the time":  {1, 0},
		"tell me the time": {1, 0},
	}}
	logger, logs := testLogger()
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	emb.err = context.DeadlineExceeded

	runner := newFakeRunner()
	runner.result = agent.Result{Text: "agent answer"}
	p, err := pipeline.New(reg, map[string]string{"what time is it": "time.now"}, logger,
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgent(runner, nil),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "what time is it right now")
	if reply.Path != tool.PathAgent || reply.Text != "agent answer" {
		t.Fatalf("reply = %+v, want the Agent fallback on embed failure", reply)
	}
	if !strings.Contains(logs.String(), `"fallthrough_reason":"embed_failed"`) {
		t.Errorf("embed failure was not logged: %s", logs.String())
	}

	reply = p.Handle(context.Background(), request(), "what time is it")
	if reply.Path != tool.PathControlPhrase || reply.Text != "It's 3:04 PM." {
		t.Fatalf("control reply = %+v, want the Control phrase to keep working", reply)
	}
}
