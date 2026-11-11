package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

const (
	testThreshold = 0.70
	testMargin    = 0.08
)

type argIn struct {
	Value string `json:"value"`
}

type fakeAgent struct {
	got   string
	reply pipeline.Reply
}

func (f *fakeAgent) Handle(_ context.Context, _ tool.Request, transcript string) pipeline.Reply {
	f.got = transcript
	return f.reply
}

type fakeConfirmation struct {
	got     string
	reply   pipeline.Reply
	prefix  string
	handled bool
}

func (f *fakeConfirmation) Resolve(_ context.Context, _ tool.Request, transcript string) pipeline.ConfirmationResult {
	f.got = transcript
	return pipeline.ConfirmationResult{Handled: f.handled, Reply: f.reply, Prefix: f.prefix}
}

// fakeEmbedder returns vectors from a fixed table keyed by input text.
type fakeEmbedder struct {
	vectors map[string][]float32
	err     error
	block   bool
	calls   int
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v, ok := f.vectors[text]
		if !ok {
			// Fail loudly instead of silently scoring an unplanned phrase.
			return nil, errors.New("fake embedder has no vector for: " + text)
		}
		out[i] = v
	}
	return out, nil
}

func semanticConfig() pipeline.SemanticConfig {
	return pipeline.SemanticConfig{
		Threshold: testThreshold,
		Margin:    testMargin,
		Joiners:   []string{"and", "then", "also"},
		Timeout:   150 * time.Millisecond,
	}
}

func newComposedPipeline(t *testing.T, reg *tool.Registry, phrases map[string]string, opts ...pipeline.Option) (*pipeline.Pipeline, *bytes.Buffer) {
	t.Helper()
	logger, logs := testLogger()
	p, err := pipeline.New(reg, phrases, logger, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p, logs
}

func newSemanticPipeline(t *testing.T, reg *tool.Registry, phrases map[string]string, emb *fakeEmbedder, opts ...pipeline.Option) (*pipeline.Pipeline, *bytes.Buffer) {
	t.Helper()
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	opts = append([]pipeline.Option{pipeline.WithSemanticRouter(router)}, opts...)
	return newComposedPipeline(t, reg, phrases, opts...)
}

func mustAdd(t *testing.T, reg *tool.Registry, tl tool.Tool[fakeIn, fakeOut]) {
	t.Helper()
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func logRecord(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %s", len(lines), logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	return record
}

func TestSemanticParaphraseReachesTimeNow(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it":           {1, 0},
		"what's the time":           {1, 0},
		"tell me the time":          {1, 0},
		"what's the time right now": {0.98, 0.199},
	}}
	p, _ := newSemanticPipeline(t, reg, nil, emb)

	reply := p.Handle(context.Background(), request(), "what's the time right now")
	if reply.Tool != clock.Name {
		t.Fatalf("tool = %q, want %q", reply.Tool, clock.Name)
	}
	if reply.Path != tool.PathFast {
		t.Fatalf("path = %q, want fast_path", reply.Path)
	}
	if want := "It's 3:04 PM."; reply.Text != want {
		t.Fatalf("reply = %q, want %q", reply.Text, want)
	}
}

func TestSemanticBelowThresholdFallsThrough(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "time"}, nil
	})
	tl.Examples = []string{"what time is it"}
	mustAdd(t, reg, tl)

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it": {1, 0},
		"hey there":       {0.69, 0.724},
	}}

	p, logs := newSemanticPipeline(t, reg, nil, emb)
	reply := p.Handle(context.Background(), request(), "hey there")
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
	if reply.Text != pipeline.PlaceholderReply {
		t.Fatalf("reply = %q, want placeholder", reply.Text)
	}
	record := logRecord(t, logs)
	if record["fallthrough_reason"] != "below_threshold" {
		t.Errorf("reason = %v, want below_threshold", record["fallthrough_reason"])
	}
	top, ok := record["top_tools"].([]any)
	if !ok || len(top) != 1 {
		t.Fatalf("top_tools = %v", record["top_tools"])
	}
}

func TestSemanticMarginFallsThrough(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	timeTool := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "time"}, nil
	})
	timeTool.Examples = []string{"what time is it"}
	weatherTool := fakeTool("test.weather", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "weather"}, nil
	})
	weatherTool.Examples = []string{"what's the weather"}
	mustAdd(t, reg, timeTool)
	mustAdd(t, reg, weatherTool)

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it":     {1, 0},
		"what's the weather":  {0.95, 0.3122}, // 0.95 with the utterance: margin 0.05
		"what time is it now": {1, 0},
	}}
	p, logs := newSemanticPipeline(t, reg, nil, emb)
	reply := p.Handle(context.Background(), request(), "what time is it now")
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
	record := logRecord(t, logs)
	if record["fallthrough_reason"] != "below_margin" {
		t.Errorf("reason = %v, want below_margin", record["fallthrough_reason"])
	}
	if margin, _ := record["margin"].(float64); margin >= testMargin {
		t.Errorf("margin = %v, want < %v", margin, testMargin)
	}
}

func TestSemanticCompoundGoesToAgent(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	timeTool := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "time"}, nil
	})
	timeTool.Examples = []string{"what time is it"}
	weatherTool := fakeTool("test.weather", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "weather"}, nil
	})
	weatherTool.Examples = []string{"what's the weather"}
	mustAdd(t, reg, timeTool)
	mustAdd(t, reg, weatherTool)

	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it":                 {1, 0},
		"what's the weather":              {0.9, 0.4359},
		"what time is it and the weather": {1, 0},
	}}
	agent := &fakeAgent{reply: pipeline.Reply{Text: "agent answer", Path: tool.PathAgent}}
	p, logs := newSemanticPipeline(t, reg, nil, emb, pipeline.WithAgentPath(agent))

	reply := p.Handle(context.Background(), request(), "what time is it and the weather")
	if reply.Text != "agent answer" {
		t.Fatalf("reply = %q, want agent answer", reply.Text)
	}
	if agent.got != "what time is it and the weather" {
		t.Errorf("agent transcript = %q", agent.got)
	}
	record := logRecord(t, logs)
	if record["fallthrough_reason"] != "compound" {
		t.Errorf("reason = %v, want compound", record["fallthrough_reason"])
	}
}

func TestSemanticIgnoresIneligibleTools(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*tool.Tool[fakeIn, fakeOut])
	}{
		{"confirm permission", func(tl *tool.Tool[fakeIn, fakeOut]) { tl.Permission = tool.PermissionConfirm }},
		{"no speak", func(tl *tool.Tool[fakeIn, fakeOut]) { tl.Speak = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := tool.NewRegistry(time.Second)
			tl := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
				return fakeOut{Value: "time"}, nil
			})
			tl.Examples = []string{"what time is it"}
			tt.mutate(&tl)
			mustAdd(t, reg, tl)

			emb := &fakeEmbedder{vectors: map[string][]float32{"what time is it": {1, 0}}}
			p, logs := newSemanticPipeline(t, reg, nil, emb)
			reply := p.Handle(context.Background(), request(), "what time is it")
			if reply.Path != tool.PathFallthrough || reply.Tool != "" {
				t.Fatalf("reply = %+v, want fallthrough without a tool", reply)
			}
			if record := logRecord(t, logs); record["fallthrough_reason"] != "no_semantic_tools" {
				t.Errorf("reason = %v, want no_semantic_tools", record["fallthrough_reason"])
			}
		})
	}
}

// argTool has an input field, so it is eligible only when it also has Extract.
func argTool(t *testing.T, reg *tool.Registry, extract func(string) (argIn, bool)) {
	t.Helper()
	tl := tool.Tool[argIn, fakeOut]{
		Name:        "test.arg",
		Description: "Fake arg tool.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in argIn) (fakeOut, error) {
			return fakeOut{Value: "set " + in.Value}, nil
		},
		Speak:    func(out fakeOut) string { return out.Value },
		Examples: []string{"set the thing"},
		Extract:  extract,
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add arg tool: %v", err)
	}
}

func TestSemanticArgsWithoutExtractIsIneligible(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, tool.Tool[argIn, fakeOut]{
		Name:        "test.arg",
		Description: "Fake arg tool.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, argIn) (fakeOut, error) {
			return fakeOut{Value: "set"}, nil
		},
		Speak:    func(out fakeOut) string { return out.Value },
		Examples: []string{"set the thing"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{"set the thing": {1, 0}}}
	p, _ := newSemanticPipeline(t, reg, nil, emb)
	reply := p.Handle(context.Background(), request(), "set the thing")
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
}

func TestSemanticExtractNotOKFallsThrough(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	argTool(t, reg, func(string) (argIn, bool) { return argIn{}, false })

	emb := &fakeEmbedder{vectors: map[string][]float32{"set the thing": {1, 0}}}
	p, logs := newSemanticPipeline(t, reg, nil, emb)
	reply := p.Handle(context.Background(), request(), "set the thing")
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
	record := logRecord(t, logs)
	if record["fallthrough_reason"] != "extract_failed" {
		t.Errorf("reason = %v, want extract_failed", record["fallthrough_reason"])
	}
	if record["extract"] != "not_ok" {
		t.Errorf("extract = %v, want not_ok", record["extract"])
	}
}

func TestSemanticExtractOKRunsFastPathWithArgs(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	argTool(t, reg, func(string) (argIn, bool) { return argIn{Value: "5 minutes"}, true })

	emb := &fakeEmbedder{vectors: map[string][]float32{"set the thing": {1, 0}}}
	p, logs := newSemanticPipeline(t, reg, nil, emb)
	reply := p.Handle(context.Background(), request(), "set the thing")
	if reply.Path != tool.PathFast {
		t.Fatalf("path = %q, want fast_path", reply.Path)
	}
	if reply.Text != "set 5 minutes" {
		t.Fatalf("reply = %q, want extracted args used", reply.Text)
	}
	if record := logRecord(t, logs); record["extract"] != "ok" {
		t.Errorf("extract = %v, want ok", record["extract"])
	}
}

func TestEmbedFailureFallsThroughWhileControlPhraseWorks(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	// Index examples with a good embedder, then fail every request embed.
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"what time is it": {1, 0}, "what's the time": {1, 0}, "tell me the time": {1, 0},
	}}
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	emb.err = errors.New("connection refused")

	p, logs := newComposedPipeline(t, reg, map[string]string{"what time is it": clock.Name}, pipeline.WithSemanticRouter(router))
	reply := p.Handle(context.Background(), request(), "what time is it right now")
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
	record := logRecord(t, logs)
	if record["fallthrough_reason"] != "embed_failed" {
		t.Errorf("reason = %v, want embed_failed", record["fallthrough_reason"])
	}
	if !strings.Contains(logs.String(), "connection refused") && !strings.Contains(asString(record["embed_error"]), "connection refused") {
		t.Errorf("embed failure was not logged: %s", logs.String())
	}

	// Control phrases keep working while embed is down.
	reply = p.Handle(context.Background(), request(), "what time is it")
	if reply.Path != tool.PathControlPhrase || reply.Tool != clock.Name {
		t.Fatalf("control reply = %+v, want control phrase route", reply)
	}
}

func TestSemanticEmbedTimeoutFallsThrough(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "time"}, nil
	})
	tl.Examples = []string{"what time is it"}
	mustAdd(t, reg, tl)

	emb := &fakeEmbedder{vectors: map[string][]float32{"what time is it": {1, 0}}}
	cfg := semanticConfig()
	cfg.Timeout = 20 * time.Millisecond
	router := pipeline.NewSemanticRouter(reg, emb, cfg)
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	emb.block = true

	p, logs := newComposedPipeline(t, reg, nil, pipeline.WithSemanticRouter(router))
	started := time.Now()
	reply := p.Handle(context.Background(), request(), "what time is it")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("routing took %s, want close to the 20ms embed timeout", elapsed)
	}
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
	record := logRecord(t, logs)
	if record["fallthrough_reason"] != "embed_failed" {
		t.Errorf("reason = %v, want embed_failed", record["fallthrough_reason"])
	}
	if !strings.Contains(asString(record["embed_error"]), "deadline exceeded") {
		t.Errorf("embed_error = %v, want deadline exceeded", record["embed_error"])
	}
}

func TestControlPhraseBeatsSemantic(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "time"}, nil
	})
	tl.Examples = []string{"what time is it"}
	mustAdd(t, reg, tl)

	emb := &fakeEmbedder{vectors: map[string][]float32{"what time is it": {1, 0}}}
	p, _ := newSemanticPipeline(t, reg, map[string]string{"what time is it": "test.time"}, emb)
	reply := p.Handle(context.Background(), request(), "what time is it")
	if reply.Path != tool.PathControlPhrase {
		t.Fatalf("path = %q, want control_phrase", reply.Path)
	}
	if emb.calls != 1 {
		t.Fatalf("embed calls = %d, want only the startup Build call", emb.calls)
	}
}

func TestAgentPrefixBeatsControlAndSemantic(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.time", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{Value: "time"}, nil
	})
	tl.Examples = []string{"what time is it"}
	mustAdd(t, reg, tl)

	emb := &fakeEmbedder{vectors: map[string][]float32{"what time is it": {1, 0}}}
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	agent := &fakeAgent{reply: pipeline.Reply{Text: "agent answer"}}
	p, logs := newComposedPipeline(t, reg, map[string]string{"what time is it": "test.time"},
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgentPrefixes([]string{"ask the agent", "agent", "hey agent"}),
		pipeline.WithAgentPath(agent),
	)

	reply := p.Handle(context.Background(), request(), "Agent, what time is it")
	if reply.Text != "agent answer" || reply.Path != tool.PathAgent {
		t.Fatalf("reply = %+v, want agent answer", reply)
	}
	if agent.got != "what time is it" {
		t.Fatalf("agent transcript = %q, want stripped request", agent.got)
	}
	record := logRecord(t, logs)
	if record["path"] != string(tool.PathAgent) {
		t.Errorf("path = %v", record["path"])
	}
}

func TestAgentPrefixWithNothingAfterAsksForRequest(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	agent := &fakeAgent{}
	p, _ := newComposedPipeline(t, reg, nil,
		pipeline.WithAgentPrefixes([]string{"ask the agent"}),
		pipeline.WithAgentPath(agent),
	)
	reply := p.Handle(context.Background(), request(), "ask the agent")
	if reply.Text != pipeline.EmptyAgentRequestReply {
		t.Fatalf("reply = %q, want %q", reply.Text, pipeline.EmptyAgentRequestReply)
	}
	if reply.Path != tool.PathAgent {
		t.Fatalf("path = %q, want agent_path", reply.Path)
	}
	if agent.got != "" {
		t.Fatalf("agent was called with %q", agent.got)
	}
}

func TestConfirmationResolverRunsBeforeEverything(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	emb := &fakeEmbedder{vectors: map[string][]float32{}}
	router := pipeline.NewSemanticRouter(reg, emb, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	confirm := &fakeConfirmation{handled: true, reply: pipeline.Reply{Text: "Timer stopped.", Tool: "test.stop"}}
	agent := &fakeAgent{reply: pipeline.Reply{Text: "agent"}}
	p, logs := newComposedPipeline(t, reg, nil,
		pipeline.WithConfirmationResolver(confirm),
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgentPrefixes([]string{"agent"}),
		pipeline.WithAgentPath(agent),
	)

	reply := p.Handle(context.Background(), request(), "yes")
	if reply.Text != "Timer stopped." {
		t.Fatalf("reply = %q, want confirmation reply", reply.Text)
	}
	if reply.Path != tool.PathConfirmation {
		t.Errorf("path = %q, want confirmation", reply.Path)
	}
	if confirm.got != "yes" {
		t.Errorf("confirmation transcript = %q", confirm.got)
	}
	if agent.got != "" {
		t.Error("agent ran before the confirmation resolver")
	}
	if record := logRecord(t, logs); record["tool"] != "test.stop" {
		t.Errorf("tool = %v", record["tool"])
	}
}

func TestNoPendingConfirmationRoutesNormally(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	confirm := &fakeConfirmation{handled: false}
	p, _ := newComposedPipeline(t, reg, map[string]string{"what time is it": clock.Name},
		pipeline.WithConfirmationResolver(confirm))

	reply := p.Handle(context.Background(), request(), "what time is it")
	if reply.Path != tool.PathControlPhrase || reply.Tool != clock.Name {
		t.Fatalf("reply = %+v, want control phrase route", reply)
	}
	if confirm.got != "what time is it" {
		t.Errorf("resolver transcript = %q", confirm.got)
	}
}

func TestDeclinedConfirmationSpeaksCancelledThenRoutes(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	confirm := &fakeConfirmation{handled: false, prefix: "Cancelled."}
	p, _ := newComposedPipeline(t, reg, map[string]string{"what time is it": clock.Name},
		pipeline.WithConfirmationResolver(confirm))

	reply := p.Handle(context.Background(), request(), "what time is it")
	if want := "Cancelled. It's 3:04 PM."; reply.Text != want {
		t.Fatalf("reply = %q, want %q", reply.Text, want)
	}
	if reply.Path != tool.PathControlPhrase || reply.Tool != clock.Name {
		t.Fatalf("reply = %+v, want the new request routed", reply)
	}
}

func TestTopThreeToolsAreLoggedInOrder(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	for _, name := range []string{"a", "b", "c", "d"} {
		name := name
		tl := fakeTool("test."+name, func(context.Context, fakeIn) (fakeOut, error) {
			return fakeOut{Value: name}, nil
		})
		tl.Examples = []string{"phrase " + name}
		mustAdd(t, reg, tl)
	}
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"phrase a":     {1, 0},
		"phrase b":     {0.9, 0.4359},
		"phrase c":     {0.8, 0.6},
		"phrase d":     {0.7, 0.714},
		"do the thing": {1, 0},
	}}
	p, logs := newSemanticPipeline(t, reg, nil, emb)
	p.Handle(context.Background(), request(), "do the thing")

	record := logRecord(t, logs)
	top, ok := record["top_tools"].([]any)
	if !ok {
		t.Fatalf("top_tools = %v", record["top_tools"])
	}
	if len(top) != 3 {
		t.Fatalf("top_tools length = %d, want 3", len(top))
	}
	first := top[0].(map[string]any)
	if first["tool"] != "test.a" {
		t.Errorf("first tool = %v, want test.a", first["tool"])
	}
	last := top[2].(map[string]any)
	if last["tool"] != "test.c" {
		t.Errorf("third tool = %v, want test.c", last["tool"])
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
