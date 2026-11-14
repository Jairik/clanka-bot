package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
)

// alertAudio configures pending alerts through their existing managers.
func (h *Handler) alertAudio(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	deps := h.alertDeps()
	audio := strings.TrimSpace(r.FormValue("audio"))
	if len(audio) > 2048 {
		http.Error(w, "Audio must be at most 2048 characters", 400)
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	var err error
	switch {
	case kind == "timer" && deps.timers != nil:
		err = deps.timers.SetAudio(r.Context(), id, audio)
	case kind == "alarm" && deps.alarms != nil:
		var out alarm.UpdateOut
		out, err = deps.alarms.Update(r.Context(), alarm.UpdateIn{ID: id, Audio: &audio})
		if err == nil && out.Matched != 1 {
			err = fmt.Errorf("alarm is no longer pending")
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	h.writeJSON(w, 200, h.buildAlertsView("Completion audio saved.", "", nil))
}

// alertCreate lets the Dashboard schedule a timer or clock alarm with media.
func (h *Handler) alertCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	deps := h.alertDeps()
	audio, label := strings.TrimSpace(r.FormValue("audio")), r.FormValue("label")
	if len(audio) > 2048 {
		http.Error(w, "Audio must be at most 2048 characters", 400)
		return
	}
	var err error
	switch r.FormValue("kind") {
	case "timer":
		seconds, e := strconv.Atoi(r.FormValue("seconds"))
		if e != nil || seconds <= 0 || seconds > 604800 || deps.timers == nil {
			http.Error(w, "Enter timer seconds from 1 to 604800", 400)
			return
		}
		_, err = deps.timers.Start(label, time.Duration(seconds)*time.Second, audio)
	case "alarm":
		clock, e := time.Parse("15:04", r.FormValue("clock"))
		if e != nil || deps.alarms == nil {
			http.Error(w, "Enter a clock time as HH:MM", 400)
			return
		}
		_, err = deps.alarms.Set(r.Context(), alarm.SetIn{Hour: clock.Hour(), Minute: clock.Minute(), Label: label, Audio: audio})
	default:
		http.Error(w, "Choose timer or alarm", 400)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	h.writeJSON(w, 200, h.buildAlertsView("Alert scheduled.", "", nil))
}
