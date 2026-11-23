package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Jairik/clanka-bot/assistant/internal/hermes"
)

// hermesRuntimeGet returns the selected provider and available models to the owner.
func (h *Handler) hermesRuntimeGet(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, h.hermesRuntime.Runtime())
}

// hermesRuntimeSave accepts only the provider and preferred cloud model.
func (h *Handler) hermesRuntimeSave(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	var selection *hermes.Selection
	if err := decoder.Decode(&selection); err != nil || selection == nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send a single valid Hermes runtime selection."})
		return
	}
	view, err := h.hermesRuntime.SetRuntime(r.Context(), *selection)
	switch {
	case err == nil:
		h.writeJSON(w, http.StatusOK, view)
	case errors.Is(err, hermes.ErrProvider), errors.Is(err, hermes.ErrCloudModel), errors.Is(err, hermes.ErrCloudKey):
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, hermes.ErrRuntimeUnavailable):
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	default:
		h.logger.Error("Hermes runtime settings write failed", "error", err)
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not save Hermes runtime settings."})
	}
}
