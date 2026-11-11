package reply_test

import (
	"context"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// TestVoiceResponderStreamsThroughTheSharedSession proves the voice loop's
// responder is the same spoken seam as the debug entry: the Agent path filler
// plays once and the answer starts streaming before generation ends.
func TestVoiceResponderStreamsThroughTheSharedSession(t *testing.T) {
	log := &eventLog{}
	speaker := newFakeSpeaker()
	routed := &fakePipeline{}
	routed.handle = func(events pipeline.Events) pipeline.Reply {
		events.AgentPath()
		events.Delta("The forecast is fine today, but it could rain later.")
		return pipeline.Reply{Text: "The forecast is fine today, but it could rain later."}
	}

	session := reply.New(routed, speaker, logSink{log}, testFiller())
	responder := voice.NewPipelineResponder(session, tool.DevicePi)
	if err := responder.Respond(context.Background(), voice.Request{Transcript: "will it rain"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	if got, want := speaker.phrases(), []string{"The forecast is fine today,", "but it could rain later."}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	if got, want := log.all(), []string{"filler-audio"}; !equal(got, want) {
		t.Fatalf("order = %q, want %q", got, want)
	}
}
