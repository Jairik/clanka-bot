package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// clauseRegistry is a small Fast-path surface whose Examples supply the
// command-leading words the clause detector uses.
func clauseRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(time.Second)
	add := func(name string, examples ...string) {
		t.Helper()
		if err := tool.Add(reg, tool.Tool[fakeIn, fakeOut]{
			Name:        name,
			Description: "Clause test tool.",
			Permission:  tool.PermissionRead,
			LocalOnly:   tool.LocalOnlyNo,
			Handler: func(context.Context, fakeIn) (fakeOut, error) {
				return fakeOut{Value: name}, nil
			},
			Speak:    func(out fakeOut) string { return out.Value },
			Examples: examples,
		}); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	add("timer.set", "set a timer for five minutes")
	add("quiet.set", "start quiet hours at 10 pm", "end quiet hours")
	add("random.pick", "pick between pizza and tacos")
	add("clock.now", "what time is it")
	add("chat.reply", "tell me a story")
	return reg
}

// clauseVectors maps every Example and test utterance to a basis vector. A
// matched utterance is identical to its Tool's Example vector, so the top
// score is 1.0 and no other Tool ties.
func clauseVectors() map[string][]float32 {
	timer := []float32{1, 0, 0, 0, 0}
	quiet := []float32{0, 1, 0, 0, 0}
	pick := []float32{0, 0, 1, 0, 0}
	clock := []float32{0, 0, 0, 1, 0}
	chat := []float32{0, 0, 0, 0, 1}
	return map[string][]float32{
		"set a timer for five minutes":            timer,
		"start quiet hours at 10 pm":              quiet,
		"end quiet hours":                         quiet,
		"pick between pizza and tacos":            pick,
		"what time is it":                         clock,
		"tell me a story":                         chat,
		"set a timer for five minutes and pause":  timer,
		"set a timer for five minutes and resume": timer,
		"set a timer for five minutes and set another timer for ten minutes": timer,
		// "tell" is a leading word from chat.reply, while the suffix itself
		// scores below the threshold: only the clause rule can catch it.
		"set a timer for five minutes and tell me a joke":     timer,
		"set a timer for an hour and a half":                  timer,
		"set a timer for five minutes called pasta and sauce": timer,
		"start quiet hours at 10 pm and end at 7":             quiet,
		"what time is it and set a timer for five minutes":    timer,
	}
}

func newClausePipeline(t *testing.T) (*pipeline.Pipeline, *fakeRunner) {
	t.Helper()
	reg := clauseRegistry(t)
	embedder := &fakeEmbedder{vectors: clauseVectors()}
	router := pipeline.NewSemanticRouter(reg, embedder, semanticConfig())
	if err := router.Build(context.Background()); err != nil {
		t.Fatalf("semantic Build: %v", err)
	}
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "agent answer"}
	p, err := pipeline.New(reg,
		map[string]string{"pause": "pause", "resume": "resume", "stop": "stop"},
		nil,
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgent(runner, nil),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p, runner
}

// TestSecondCommandClauseRoutesToAgent covers the verified escape and the
// same-Tool repeat without letting a label, number continuation or schedule
// continuation fall off the Fast path.
func TestSecondCommandClauseRoutesToAgent(t *testing.T) {
	p, runner := newClausePipeline(t)
	compound := []string{
		"set a timer for five minutes and pause",
		"set a timer for five minutes and resume",
		"set a timer for five minutes and set another timer for ten minutes",
		"set a timer for five minutes and tell me a joke",
		"what time is it and set a timer for five minutes",
	}
	for _, utterance := range compound {
		reply := p.Handle(context.Background(), request(), utterance)
		if reply.Path != tool.PathAgent {
			t.Errorf("%q path = %q, want the Agent path (%q)", utterance, reply.Path, reply.Text)
		}
	}
	if got := len(runner.calls()); got != len(compound) {
		t.Fatalf("Agent calls = %d, want %d", got, len(compound))
	}
}

func TestSingleRequestsStayOnTheFastPath(t *testing.T) {
	p, _ := newClausePipeline(t)
	fast := map[string]string{
		"set a timer for five minutes":                        "timer.set",
		"set a timer for an hour and a half":                  "timer.set",
		"set a timer for five minutes called pasta and sauce": "timer.set",
		"start quiet hours at 10 pm and end at 7":             "quiet.set",
		"pick between pizza and tacos":                        "random.pick",
		"what time is it":                                     "clock.now",
	}
	for utterance, want := range fast {
		reply := p.Handle(context.Background(), request(), utterance)
		if reply.Path != tool.PathFast || reply.Tool != want {
			t.Errorf("%q = path %q tool %q, want fast path %q", utterance, reply.Path, reply.Tool, want)
		}
	}
}
