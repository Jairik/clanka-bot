// Equalizer controls use the same durable settings store as the voice settings.
package dashboard

import (
	"github.com/Jairik/clanka-bot/assistant/internal/equalizer"
	"net/http"
)

type equalizerView struct {
	Profile equalizer.Profile
	Custom  equalizer.Profile
	Error   string
}

func (h *Handler) equalizerRead() equalizerView {
	if h.settings == nil {
		return equalizerView{Error: "Equalizer settings unavailable."}
	}
	p, _ := equalizer.Parse(h.settings.String(equalizer.Key))
	custom, _ := equalizer.Parse(h.settings.String(equalizer.CustomKey))
	return equalizerView{Profile: p, Custom: custom}
}
func (h *Handler) equalizerGet(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, http.StatusOK, h.equalizerRead())
}
func (h *Handler) equalizerSet(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	p, err := equalizer.Parse(r.FormValue("profile"))
	if err != nil {
		v := h.equalizerRead()
		v.Error = err.Error()
		h.writeJSON(w, 400, v)
		return
	}
	values := map[string]any{equalizer.Key: equalizer.Encode(p)}
	if p.Preset == "Custom" {
		values[equalizer.CustomKey] = equalizer.Encode(p)
	}
	if err = h.settings.SetMany(r.Context(), values); err != nil {
		v := h.equalizerRead()
		v.Error = "Could not save equalizer."
		h.writeJSON(w, 500, v)
		return
	}
	h.writeJSON(w, 200, h.equalizerRead())
}
