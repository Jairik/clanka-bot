// The opt-in that lets a Dashboard answer speak on the Pi. It is an ordinary
// live setting, so the top-bar toggle and the request path read one value, and
// the spoken reply goes through the same reply Session (and therefore the same
// Focus, announcement queue and Quiet hours gain) as a voice turn.
package dashboard

import (
	"net/http"
	"strconv"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// SpeakRepliesKey is the persisted opt-in. Off by default: a Dashboard answer
// is shown on screen and the Pi stays quiet unless the owner asks otherwise.
const SpeakRepliesKey = "dashboard.speak_replies"

// SpeakRepliesDefinition registers the setting with the one settings service.
func SpeakRepliesDefinition() settings.Definition {
	return settings.Bool(SpeakRepliesKey, false, settings.WithDescription(
		"Speak the Dashboard chat's reply on the Pi speaker. Off by default; the answer is always shown on screen."))
}

// speaking reports whether this request should be answered with Pi audio.
func (h *Handler) speaking() bool {
	return h.spoken != nil && h.settings != nil && h.settings.Bool(SpeakRepliesKey)
}

// speakView is the toggle's answer: the state the server now has.
type speakView struct {
	Speak bool
}

func (h *Handler) speakToggle(w http.ResponseWriter, r *http.Request) {
	if h.spoken == nil || h.settings == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Could not read the form.", http.StatusBadRequest)
		return
	}
	on, err := strconv.ParseBool(r.FormValue("value"))
	if err != nil {
		http.Error(w, "Send value=true or value=false.", http.StatusBadRequest)
		return
	}
	if _, err := h.settings.Set(r.Context(), SpeakRepliesKey, on); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.writeJSON(w, http.StatusOK, speakView{Speak: on})
}
