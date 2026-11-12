package reply_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// blockingAgent keeps the model call running until its request is cancelled and
// records the cancellation, so a speaker-only Stop cannot pass this test.
type blockingAgent struct {
	history *agent.History
	started chan struct{}
	stopped chan struct{}

	mu  sync.Mutex
	ctx context.Context
}

func newBlockingAgent() *blockingAgent {
	return &blockingAgent{
		history: agent.NewHistory(10, 5*time.Minute, nil),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (a *blockingAgent) Run(ctx context.Context, _ tool.Request, _ string, onDelta func(string)) agent.Result {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	onDelta("The answer is forty two, and")
	close(a.started)
	<-ctx.Done()
	close(a.stopped)
	return agent.Result{Text: "The answer is forty two, and that is final.", Cancelled: true}
}

func (a *blockingAgent) History() *agent.History { return a.history }

func (a *blockingAgent) requestContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctx
}

// blockingTTS holds playback until the turn is cancelled, like the real
// speaker waiting on TTS PCM.
type blockingTTS struct {
	started chan struct{}
	once    sync.Once
}

func (s *blockingTTS) SayText(ctx context.Context, _ string) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

// TestStopFromAConcurrentInputCancelsTheOriginalReplyNotTheStopRequest is the
// lifecycle rule for shared phrases: the stop is routed to the reply that held
// Focus, cancelling its model stream and TTS, while the stop request itself
// completes normally.
func TestStopFromAConcurrentInputCancelsTheOriginalReplyNotTheStopRequest(t *testing.T) {
	var registry focus.Registry
	runner := newBlockingAgent()
	speaker := &blockingTTS{started: make(chan struct{})}

	p, err := pipeline.New(tool.NewRegistry(time.Second), map[string]string{"stop": "stop"},
		slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		pipeline.WithAgent(runner, nil),
		pipeline.WithFocus(&registry),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	session := reply.New(p, speaker, nil, nil, reply.WithFocus(&registry))

	originalDone := make(chan error, 1)
	go func() {
		_, err := session.Ask(context.Background(), tool.Request{ID: "original", Device: tool.DevicePi}, "tell me a long story")
		originalDone <- err
	}()

	select {
	case <-speaker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the original reply never started speaking")
	}

	stopCtx := context.Background()
	stopReply, stopErr := session.Ask(stopCtx, tool.Request{ID: "stop", Device: tool.DeviceSatellite}, "stop")
	if stopErr != nil {
		t.Fatalf("stop request failed: %v", stopErr)
	}
	if stopReply.Path != tool.PathFocusIntent {
		t.Fatalf("stop path = %q, want %q", stopReply.Path, tool.PathFocusIntent)
	}
	if stopCtx.Err() != nil {
		t.Fatalf("stop request was cancelled by its own intent: %v", stopCtx.Err())
	}

	select {
	case err := <-originalDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("original reply error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the original reply did not stop")
	}

	select {
	case <-runner.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the model stream kept running after stop; a speaker-only stop is insufficient")
	}
	if ctx := runner.requestContext(); ctx == nil || ctx.Err() == nil {
		t.Fatal("the model request context was not cancelled")
	}
	if _, handled := registry.HandleIntent(context.Background(), focus.Stop); handled {
		t.Fatal("a reply holder survived both turns")
	}
}

// TestStopWhileNothingHoldsFocusFallsThrough keeps the optional seam honest at
// the session level: with no Focus holder the phrase is routed normally.
func TestStopWhileNothingHoldsFocusFallsThrough(t *testing.T) {
	var registry focus.Registry
	runner := newFakeAgent()
	runner.result = agent.Result{Text: "There is nothing to stop."}

	p, err := pipeline.New(tool.NewRegistry(time.Second), map[string]string{"stop": "stop"},
		slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		pipeline.WithAgent(runner, nil),
		pipeline.WithFocus(&registry),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	session := reply.New(p, newFakeSpeaker(), nil, nil, reply.WithFocus(&registry))

	result, err := session.Ask(context.Background(), request(), "stop")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.Path != tool.PathAgent {
		t.Fatalf("path = %q, want the normal Agent fall-through", result.Path)
	}
	if result.Text != "There is nothing to stop." {
		t.Fatalf("reply = %q", result.Text)
	}
}

// fakeAgent is the smallest AgentRunner for the fall-through test.
type fakeAgent struct {
	history *agent.History
	result  agent.Result
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{history: agent.NewHistory(10, 5*time.Minute, nil)}
}

func (a *fakeAgent) Run(context.Context, tool.Request, string, func(string)) agent.Result {
	return a.result
}

func (a *fakeAgent) History() *agent.History { return a.history }
