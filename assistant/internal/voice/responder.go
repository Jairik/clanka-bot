package voice

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Handler routes one transcript to a reply. pipeline.Pipeline satisfies it.
type Handler interface {
	Handle(ctx context.Context, req tool.Request, transcript string) pipeline.Reply
}

// Speaker speaks reply text. speech.Speaker satisfies it through SayText.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// PipelineResponder is the production Responder: run the transcript through
// the normal pipeline, then speak (or merely return) the reply. Cancelling ctx
// abandons both routing and playback, which is the Barge-in seam for #25.
type PipelineResponder struct {
	handler Handler
	speaker Speaker
	device  tool.Device
}

// NewPipelineResponder builds a responder. A nil speaker returns the reply
// without playing it, which is how the voice loop runs when TTS is disabled.
func NewPipelineResponder(handler Handler, speaker Speaker, device tool.Device) *PipelineResponder {
	return &PipelineResponder{handler: handler, speaker: speaker, device: device}
}

// Respond handles one utterance and speaks the answer.
func (r *PipelineResponder) Respond(ctx context.Context, transcript string) error {
	reply := r.handler.Handle(ctx, tool.Request{
		ID:     tool.NewRequestID(),
		Path:   tool.PathFallthrough,
		Device: r.device,
	}, transcript)
	if r.speaker == nil {
		return nil
	}
	return r.speaker.SayText(ctx, reply.Text)
}
