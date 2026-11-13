package dashboard

import (
	"context"
	"net/http"
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
		http.Error(w, "Could not record: microphone busy or unavailable. Try again when the assistant is idle.", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pcm)
}
