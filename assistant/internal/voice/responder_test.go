package voice_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// fakeAsker records the request the responder builds for the shared Session.
type fakeAsker struct {
	transcript string
	request    tool.Request
	ended      time.Time
	reply      pipeline.Reply
	err        error
}

func (a *fakeAsker) AskFrom(ctx context.Context, req tool.Request, transcript string, ended time.Time) (pipeline.Reply, error) {
	a.request = req
	a.transcript = transcript
	a.ended = ended
	return a.reply, a.err
}

func TestPipelineResponderRoutesThroughTheSession(t *testing.T) {
	session := &fakeAsker{reply: pipeline.Reply{Text: "It is noon."}}
	responder := voice.NewPipelineResponder(session, tool.DevicePi)
	ended := time.Now()

	if err := responder.Respond(context.Background(), voice.Request{Transcript: "what time is it", Ended: ended}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if session.transcript != "what time is it" {
		t.Errorf("session transcript = %q", session.transcript)
	}
	if !session.ended.Equal(ended) {
		t.Errorf("Filler clock start = %v, want the endpointer time %v", session.ended, ended)
	}
	if session.request.ID == "" {
		t.Error("request ID is empty")
	}
	if session.request.Device != tool.DevicePi {
		t.Errorf("device = %q, want pi", session.request.Device)
	}
}

func TestPipelineResponderWithoutASessionIsSilent(t *testing.T) {
	responder := voice.NewPipelineResponder(nil, tool.DevicePi)
	if err := responder.Respond(context.Background(), voice.Request{Transcript: "hi"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
}

func TestPipelineResponderReportsPlaybackFailure(t *testing.T) {
	session := &fakeAsker{reply: pipeline.Reply{Text: "ok"}, err: errors.New("sink gone")}
	responder := voice.NewPipelineResponder(session, tool.DevicePi)
	if err := responder.Respond(context.Background(), voice.Request{Transcript: "hi"}); err == nil {
		t.Fatal("Respond succeeded, want playback error")
	}
}
