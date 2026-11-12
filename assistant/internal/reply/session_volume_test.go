package reply_test

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

// gainSpeaker records the software gain attached to each spoken phrase.
type gainSpeaker struct {
	mu    sync.Mutex
	gains []int
	said  []string
}

func (s *gainSpeaker) SayText(ctx context.Context, text string) error {
	if hook := speech.FirstAudioHook(ctx); hook != nil {
		hook()
	}
	s.mu.Lock()
	s.gains = append(s.gains, speech.GainFrom(ctx))
	s.said = append(s.said, text)
	s.mu.Unlock()
	return nil
}

func (s *gainSpeaker) values() ([]int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.gains...), append([]string(nil), s.said...)
}

// gainSink records the context gain each Filler audio stream opens with.
type gainSink struct {
	mu    sync.Mutex
	gains []int
}

func (s *gainSink) Open(ctx context.Context, _ audio.Format) (io.WriteCloser, error) {
	s.mu.Lock()
	s.gains = append(s.gains, speech.GainFrom(ctx))
	s.mu.Unlock()
	return discardCloser{}, nil
}

func (s *gainSink) values() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.gains...)
}

type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

func TestReplyVolumeSourceAppliesOncePerTurn(t *testing.T) {
	speaker := &gainSpeaker{}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.Delta("This reply is quiet.")
		return pipeline.Reply{Text: "This reply is quiet.", Tool: "test"}
	}
	calls := 0
	var mu sync.Mutex
	session := reply.New(routed, speaker, nil, nil, reply.WithVolumeSource(func() int {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return 35
	}))
	for i := 0; i < 2; i++ {
		if _, err := session.Ask(context.Background(), request(), "hello"); err != nil {
			t.Fatalf("Ask: %v", err)
		}
	}
	gains, said := speaker.values()
	if len(gains) == 0 || len(said) != 2 {
		t.Fatalf("speaker = %v / %v, want two spoken replies", gains, said)
	}
	for _, gain := range gains {
		if gain != 35 {
			t.Fatalf("spoken gains = %v, want every phrase at 35", gains)
		}
	}
	if calls != 2 {
		t.Fatalf("volume source called %d times, want once per turn", calls)
	}
}

func TestReplyVolumeSourceAppliesToTheFiller(t *testing.T) {
	speaker := &gainSpeaker{}
	sink := &gainSink{}
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		return pipeline.Reply{Text: "A slow answer.", Tool: "test"}
	}
	session := reply.New(routed, speaker, sink, testFiller(), reply.WithVolumeSource(func() int { return 40 }))
	if _, err := session.Ask(context.Background(), request(), "hello"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	gains := sink.values()
	if len(gains) != 1 || gains[0] != 40 {
		t.Fatalf("filler gains = %v, want one at 40", gains)
	}
	if speechGains, _ := speaker.values(); len(speechGains) == 0 || speechGains[0] != 40 {
		t.Fatalf("reply gains = %v, want the reply at 40", speechGains)
	}
}
