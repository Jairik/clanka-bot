package voice_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

type fakeHandler struct {
	transcript string
	request    tool.Request
	reply      pipeline.Reply
}

func (h *fakeHandler) Handle(ctx context.Context, req tool.Request, transcript string) pipeline.Reply {
	h.request = req
	h.transcript = transcript
	return h.reply
}

type fakeSpeaker struct {
	text string
	err  error
}

func (s *fakeSpeaker) SayText(ctx context.Context, text string) error {
	s.text = text
	return s.err
}

func TestPipelineResponderRoutesAndSpeaksTheReply(t *testing.T) {
	handler := &fakeHandler{reply: pipeline.Reply{Text: "It is noon."}}
	speaker := &fakeSpeaker{}
	responder := voice.NewPipelineResponder(handler, speaker, tool.DevicePi)

	if err := responder.Respond(context.Background(), "what time is it"); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if handler.transcript != "what time is it" {
		t.Errorf("handler transcript = %q", handler.transcript)
	}
	if handler.request.ID == "" {
		t.Error("request ID is empty")
	}
	if handler.request.Device != tool.DevicePi {
		t.Errorf("device = %q, want pi", handler.request.Device)
	}
	if speaker.text != "It is noon." {
		t.Errorf("spoken = %q", speaker.text)
	}
}

func TestPipelineResponderWithoutASpeakerIsSilent(t *testing.T) {
	handler := &fakeHandler{reply: pipeline.Reply{Text: "ok"}}
	responder := voice.NewPipelineResponder(handler, nil, tool.DevicePi)
	if err := responder.Respond(context.Background(), "hi"); err != nil {
		t.Fatalf("Respond: %v", err)
	}
}

func TestPipelineResponderReportsSpeechFailure(t *testing.T) {
	handler := &fakeHandler{reply: pipeline.Reply{Text: "ok"}}
	speaker := &fakeSpeaker{err: errors.New("sink gone")}
	responder := voice.NewPipelineResponder(handler, speaker, tool.DevicePi)
	if err := responder.Respond(context.Background(), "hi"); err == nil {
		t.Fatal("Respond succeeded, want speech error")
	}
}
