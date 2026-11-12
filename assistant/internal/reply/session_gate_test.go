package reply_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/playback"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// sentencePipeline streams two sentences, so the player sees two speakable
// chunks and exactly one boundary between them.
type sentencePipeline struct{}

func (sentencePipeline) HandleEvents(_ context.Context, req tool.Request, _ string, events pipeline.Events) pipeline.Reply {
	events.Delta("First sentence. ")
	events.Delta("Second sentence.")
	return pipeline.Reply{RequestID: req.ID, Text: "First sentence. Second sentence."}
}

// sentenceSpeaker holds the first sentence until the test releases it, then
// records the second. It reports every phrase start in order.
type sentenceSpeaker struct {
	started      chan string
	releaseFirst chan struct{}
	mu           sync.Mutex
	said         []string
}

func newSentenceSpeaker() *sentenceSpeaker {
	return &sentenceSpeaker{started: make(chan string, 4), releaseFirst: make(chan struct{})}
}

func (s *sentenceSpeaker) SayText(ctx context.Context, text string) error {
	s.mu.Lock()
	s.said = append(s.said, text)
	s.mu.Unlock()
	s.started <- text
	if text == "First sentence." {
		select {
		case <-s.releaseFirst:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *sentenceSpeaker) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.said...)
}

// TestReplyYieldsTheSpeakerAtASentenceBoundary is the #52 integration seam: a
// Ring waits for the current sentence, then the reply's next sentence waits
// for the Ring. The turn is not cancelled; it resumes and completes normally.
func TestReplyYieldsTheSpeakerAtASentenceBoundary(t *testing.T) {
	ctx := context.Background()
	gate := playback.NewGate()
	speaker := newSentenceSpeaker()
	session := reply.New(sentencePipeline{}, speaker, nil, nil, reply.WithSpeechGate(gate))

	done := make(chan error, 1)
	go func() {
		_, err := session.Ask(ctx, tool.Request{ID: "reply"}, "tell me two things")
		done <- err
	}()

	select {
	case text := <-speaker.started:
		if text != "First sentence." {
			t.Fatalf("first phrase = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first sentence never started")
	}

	// Ask for the floor while the first sentence is still playing.
	registered := make(chan struct{})
	ringReady := make(chan func(), 1)
	go func() {
		close(registered)
		release, err := gate.WaitForReplySentence(ctx)
		if err != nil {
			ringReady <- nil
			return
		}
		ringReady <- release
	}()
	<-registered
	time.Sleep(20 * time.Millisecond)
	if gate.RingActive() {
		t.Fatal("the Ring took the floor before the sentence ended")
	}

	close(speaker.releaseFirst)
	var release func()
	select {
	case release = <-ringReady:
	case <-time.After(2 * time.Second):
		t.Fatal("the Ring did not get the floor after the sentence ended")
	}
	if release == nil {
		t.Fatal("the Ring waiter failed")
	}

	// The reply's second sentence must now wait behind the Ring.
	select {
	case text := <-speaker.started:
		t.Fatalf("%q started while the Ring owned the floor", text)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case text := <-speaker.started:
		if text != "Second sentence." {
			t.Fatalf("second phrase = %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply did not resume after the Ring")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply did not finish")
	}
	if got := speaker.all(); len(got) != 2 {
		t.Fatalf("spoken = %q, want both sentences exactly once", got)
	}
}
