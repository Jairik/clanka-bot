package reply_test

import (
	"context"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

// ignoreReply is a pipeline answer for an ignored turn: the model streamed the
// token in the given deltas, and the Agent marked the turn ignored.
func ignoreReply(deltas ...string) *fakePipeline {
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		for _, delta := range deltas {
			events.Delta(delta)
		}
		return pipeline.Reply{Path: "agent", Ignored: true}
	}
	return routed
}

func ignoreCue() *fakeCues {
	return &fakeCues{
		clip: sound.Clip{PCM: []byte{1, 2, 3, 4}, Format: audio.Format{Rate: 16000, Width: 2, Channels: 1}},
		ok:   true,
	}
}

// TestIgnoreTokenIsNeverSpokenAndPlaysTheSoftCue covers the streamed token,
// including the pieces arriving separately: no words reach TTS and the ignore
// role is the only Cue requested (#498).
func TestIgnoreTokenIsNeverSpokenAndPlaysTheSoftCue(t *testing.T) {
	tests := []struct {
		name   string
		deltas []string
	}{
		{name: "one delta", deltas: []string{agent.IgnoreToken}},
		{name: "split across deltas", deltas: []string{"<", "ign", "ore", ">"}},
		{name: "surrounding whitespace", deltas: []string{" ", "<igno", "re>\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := &eventLog{}
			cues := ignoreCue()
			session := reply.New(ignoreReply(test.deltas...), speakingLog{log}, cueSink{log}, nil, reply.WithCues(cues))

			got, err := session.Ask(context.Background(), request(), "who left the fridge open")
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if !got.Ignored {
				t.Fatal("Ignored = false")
			}
			if got := log.all(); len(got) != 1 || got[0] != "error-cue" {
				t.Fatalf("playback = %q, want only the soft Cue", got)
			}
			if len(cues.roles) != 1 || cues.roles[0] != sound.RoleIgnore {
				t.Fatalf("cue roles = %v, want only the ignore role", cues.roles)
			}
		})
	}
}

// TestIgnoredTurnWithNoCueIsFullySilent proves the cue can be turned off: with
// no Cue source, or a none assignment, nothing at all plays.
func TestIgnoredTurnWithNoCueIsFullySilent(t *testing.T) {
	log := &eventLog{}
	session := reply.New(ignoreReply(agent.IgnoreToken), speakingLog{log}, cueSink{log}, nil)
	if _, err := session.Ask(context.Background(), request(), "mumbling"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("playback = %q, want complete silence", got)
	}
}

// TestBrokenIgnorePrefixIsSpokenNormally proves the gate only withholds the
// token itself: a reply that merely starts like it is released and spoken.
func TestBrokenIgnorePrefixIsSpokenNormally(t *testing.T) {
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.Delta("<ign")
		events.Delta("x is a tag, not a signal.")
		return pipeline.Reply{Text: "<ignx is a tag, not a signal."}
	}
	speaker := newFakeSpeaker()
	session := reply.New(routed, speaker, nil, nil, reply.WithCues(ignoreCue()))
	if _, err := session.Ask(context.Background(), request(), "what is that tag"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// The first chunk is flushed at a clause boundary, so the comma starts a
	// second spoken chunk; what matters is that the text was not withheld.
	want := []string{"<ignx is a tag,", "not a signal."}
	if got := speaker.phrases(); !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}
