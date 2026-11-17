package voice

import (
	"context"
	"errors"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// ErrSessionSuspended reports that the turn ended the session instead of
// answering, as a standalone "stop" does (#501). The loop closes the follow-up
// window and clears the shared history; this is not a failure to log.
var ErrSessionSuspended = errors.New("voice: session suspended")

// Request is one utterance handed from the loop to the responder: the
// transcript, when the endpointer fired, and the wake model that started the
// turn. The reply Filler clock starts at Ended, so a slow transcription does
// not delay the cue. WakeWord is empty for a turn that was not started by a
// model (the debug entry and Dashboard typed requests).
type Request struct {
	Transcript string
	Ended      time.Time
	WakeWord   string
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
	reply, err := r.session.AskFrom(ctx, tool.Request{
		ID:       id,
		Path:     tool.PathFallthrough,
		Device:   r.device,
		WakeWord: req.WakeWord,
	}, req.Transcript, req.Ended)
	if err != nil {
		return err
	}
	if reply.Suspend {
		return ErrSessionSuspended
	}
	return nil
}
