package reply_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

type spokenRate struct {
	text string
	rate float64
}

// rateSpeaker records the per-utterance rate carried on each Say context.
type rateSpeaker struct {
	mu     sync.Mutex
	spoken []spokenRate
}

func (s *rateSpeaker) SayText(ctx context.Context, text string) error {
	if hook := speech.FirstAudioHook(ctx); hook != nil {
		hook()
	}
	s.mu.Lock()
	s.spoken = append(s.spoken, spokenRate{text: text, rate: speech.RateFrom(ctx)})
	s.mu.Unlock()
	return nil
}

// TestSessionAppliesRateOnlyToTheReplayText proves a replay's rate is scoped to
// its one item: the final text carries the override while a Confirmation prefix
// (and, by the same per-item rule, a Filler) keeps the setting's rate.
func TestSessionAppliesRateOnlyToTheReplayText(t *testing.T) {
	rate := 0.6
	fp := &fakePipeline{handle: func(events pipeline.Events) pipeline.Reply {
		// A dropped Confirmation's lead-in is spoken before the routed answer.
		events.Prefix("Cancelled.")
		return pipeline.Reply{Text: "The answer.", SpeechRate: rate}
	}}
	speaker := &rateSpeaker{}
	session := reply.New(fp, speaker, nil, nil)
	if _, err := session.Ask(context.Background(), request(), "say that again slower"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	speaker.mu.Lock()
	got := append([]spokenRate(nil), speaker.spoken...)
	speaker.mu.Unlock()
	want := []spokenRate{{text: "Cancelled.", rate: 0}, {text: "The answer.", rate: rate}}
	if len(got) != len(want) {
		t.Fatalf("spoken = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].text != want[i].text || got[i].rate != want[i].rate {
			t.Fatalf("spoken[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSessionOmitsRateForAnOrdinaryReply proves a reply with no override (rate
// 0) reaches TTS without a context rate, inheriting the setting.
func TestSessionOmitsRateForAnOrdinaryReply(t *testing.T) {
	fp := &fakePipeline{handle: func(pipeline.Events) pipeline.Reply {
		return pipeline.Reply{Text: "A normal answer."}
	}}
	speaker := &rateSpeaker{}
	session := reply.New(fp, speaker, nil, nil)
	if _, err := session.Ask(context.Background(), request(), "what time is it"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	speaker.mu.Lock()
	got := append([]spokenRate(nil), speaker.spoken...)
	speaker.mu.Unlock()
	if len(got) != 1 || got[0].text != "A normal answer." || got[0].rate != 0 {
		t.Fatalf("spoken = %+v, want one ordinary reply at the setting's rate", got)
	}
}
