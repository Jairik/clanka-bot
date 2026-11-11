package voice

import (
	"context"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Asker answers one transcript out loud. reply.Session satisfies it and is the
// single response seam shared by the voice loop and the debug entry.
type Asker interface {
	Ask(ctx context.Context, req tool.Request, transcript string) (pipeline.Reply, error)
}

// PipelineResponder is the production Responder: it hands the transcript to
// the shared reply Session, which owns filler, streaming and playback.
// Cancelling ctx abandons routing and playback, the Barge-in seam for #25.
type PipelineResponder struct {
	session Asker
	device  tool.Device
}

// NewPipelineResponder builds a responder around the shared Session. A nil
// Session yields a silent responder that still accepts transcripts.
func NewPipelineResponder(session Asker, device tool.Device) *PipelineResponder {
	return &PipelineResponder{session: session, device: device}
}

// Respond handles one utterance and speaks the answer through the Session.
func (r *PipelineResponder) Respond(ctx context.Context, transcript string) error {
	if r.session == nil {
		return nil
	}
	_, err := r.session.Ask(ctx, tool.Request{
		ID:     tool.NewRequestID(),
		Path:   tool.PathFallthrough,
		Device: r.device,
	}, transcript)
	return err
}
