package reply_test

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
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

// pcmSink records the PCM written to it, so a cue's software volume is visible.
type pcmSink struct {
	mu  sync.Mutex
	pcm []byte
}

func (s *pcmSink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	return &pcmWriter{sink: s}, nil
}

type pcmWriter struct{ sink *pcmSink }

func (w *pcmWriter) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	w.sink.pcm = append(w.sink.pcm, p...)
	w.sink.mu.Unlock()
	return len(p), nil
}

func (w *pcmWriter) Close() error { return nil }

func TestCueScaleAppliesTheQuietGainToTheUnavailableCue(t *testing.T) {
	sink := &pcmSink{}
	cues := &fakeCues{
		clip: sound.Clip{PCM: []byte{0xe8, 0x03}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}},
		ok:   true,
	}
	session := reply.New(cueReply(), &gainSpeaker{}, sink, nil,
		reply.WithCues(cues),
		reply.WithCueScale(func(clip sound.Clip) sound.Clip {
			// The same shape quiet.Service.Clip returns at 50%.
			clip.PCM = []byte{0xf4, 0x01}
			return clip
		}),
	)
	if _, err := session.Ask(context.Background(), request(), "ask the agent"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	sink.mu.Lock()
	got := append([]byte(nil), sink.pcm...)
	sink.mu.Unlock()
	if len(got) != 2 || got[0] != 0xf4 || got[1] != 0x01 {
		t.Fatalf("cue PCM = %v, want the scaled clip", got)
	}
}
