package reply_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// streamingAgent emits the start of an answer, waits until that start is heard,
// then finishes, proving generation and playback overlap.
type streamingAgent struct {
	history     *agent.History
	firstPlayed chan struct{}
}

func (a *streamingAgent) Run(_ context.Context, _ tool.Request, _ string, onDelta func(string)) agent.Result {
	onDelta("The answer is forty two, and")
	select {
	case <-a.firstPlayed:
	case <-time.After(2 * time.Second):
		return agent.Result{Text: "timed out waiting for playback"}
	}
	onDelta(" that is final.")
	return agent.Result{Text: "The answer is forty two, and that is final."}
}

func (a *streamingAgent) History() *agent.History { return a.history }

func TestTypedAgentQuestionIsHeardBeforeGenerationFinishes(t *testing.T) {
	runner := &streamingAgent{
		history:     agent.NewHistory(10, 5*time.Minute, nil),
		firstPlayed: make(chan struct{}),
	}
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger,
		pipeline.WithAgent(runner, []string{"ask the agent"}))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	speaker := newFakeSpeaker()
	go func() {
		if chunk, ok := <-speaker.saidCh; ok && chunk != "" {
			close(runner.firstPlayed)
		}
	}()

	session := reply.New(p, speaker, nil, nil)
	result, err := session.Ask(context.Background(), request(), "ask the agent what the answer is")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Text != "The answer is forty two, and that is final." {
		t.Fatalf("reply = %q", result.Text)
	}
	want := []string{"The answer is forty two,", "and that is final."}
	if got := speaker.phrases(); !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}
