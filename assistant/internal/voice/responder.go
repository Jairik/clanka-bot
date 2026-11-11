package voice

import (
	"context"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Request is one utterance handed from the loop to the responder: the
// transcript and when the endpointer fired. The reply Filler clock starts at
// Ended, so a slow transcription does not delay the cue.
type Request struct {
	Transcript string
	Ended      time.Time
}

// Asker answers one utterance out loud. reply.Session satisfies it and is the
// single response seam shared by the voice loop and the debug entry.
type Asker interface {
	AskFrom(ctx context.Context, req tool.Request, transcript string, ended time.Time) (pipeline.Reply, error)
}

// PipelineResponder is the production Responder: it hands the utterance to the
// shared reply Session, which owns filler, streaming and playback.
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
func (r *PipelineResponder) Respond(ctx context.Context, req Request) error {
	if r.session == nil {
		return nil
	}
	// Reuse the request ID the voice loop already logged so voice, session,
	// Agent and playback lines correlate (#27).
	id := latency.RequestID(ctx)
	if id == "" {
		id = tool.NewRequestID()
	}
	_, err := r.session.AskFrom(ctx, tool.Request{
		ID:     id,
		Path:   tool.PathFallthrough,
		Device: r.device,
	}, req.Transcript, req.Ended)
	return err
}
