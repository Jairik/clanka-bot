package dashboard

import (
	"context"
	"errors"
	"net/http"

	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// SampleRecorder captures a bounded private take from the Pi's configured microphone.
type SampleRecorder interface {
	RecordSample(context.Context) ([]byte, error)
}

// voiceSamplesCapture returns raw PCM for local quality checks; keeping remains a separate action.
func (h *Handler) voiceSamplesCapture(w http.ResponseWriter, r *http.Request) {
	if h.sampleRecorder == nil {
		http.Error(w, "Pi microphone recording is unavailable.", http.StatusServiceUnavailable)
		return
	}
	pcm, err := h.sampleRecorder.RecordSample(r.Context())
	if err != nil {
		h.writeSampleCaptureError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pcm)
}

// writeSampleCaptureError maps a recorder failure to an honest, fixed
// user-facing sentence. The body stays plain text because SampleBatch.tsx
// renders the raw response text, so a JSON body would appear literally on the
// page. The detail of an unexpected failure is logged only, never returned.
func (h *Handler) writeSampleCaptureError(w http.ResponseWriter, r *http.Request, err error) {
	// The browser left (tab closed, navigation away): there is no client to
	// answer, so write nothing.
	if r.Context().Err() != nil {
		return
	}
	switch {
	case errors.Is(err, voice.ErrMicBusy):
		http.Error(w, "Microphone is busy. Wait for the current turn or take to finish.", http.StatusConflict)
	case errors.Is(err, voice.ErrSampleOverflow):
		http.Error(w, "That take overflowed. Redo it.", http.StatusConflict)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "The microphone did not deliver audio in time.", http.StatusGatewayTimeout)
	case errors.Is(err, context.Canceled):
		// The recorder's own context was cancelled but the request is still
		// alive; treat it like a client departure and answer nothing.
		return
	default:
		h.logger.Error("voice sample capture failed", "err", err)
		http.Error(w, "The microphone could not be recorded. Check the Pi and try again.", http.StatusInternalServerError)
	}
}
