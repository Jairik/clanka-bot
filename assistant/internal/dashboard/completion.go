package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/alarm"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

var (
	errAlarmNotPending = errors.New("alarm is no longer pending")
	errAlarmAmbiguous  = errors.New("ambiguous alarm")
)

// alertError maps alert manager errors to HTTP responses, keeping internal
// error details in the log.
func (h *Handler) alertError(w http.ResponseWriter, action string, err error) {
	if err == nil {
		return
	}
	// ToolError carries user-facing spoken validation text that is safe to echo.
	var toolErr *tool.ToolError
	if errors.As(err, &toolErr) && strings.TrimSpace(toolErr.Say) != "" {
		http.Error(w, strings.TrimSpace(toolErr.Say), http.StatusBadRequest)
		return
	}
	// Known gone or ambiguous states map to conflict rather than internal error.
	switch {
	case errors.Is(err, timer.ErrNotPending) || err.Error() == "timer is no longer pending":
		http.Error(w, "That timer is no longer pending.", http.StatusConflict)
		return
	case errors.Is(err, errAlarmNotPending) || err.Error() == "alarm is no longer pending":
		http.Error(w, "That alarm is no longer pending.", http.StatusConflict)
		return
	case errors.Is(err, errAlarmAmbiguous) || strings.Contains(strings.ToLower(err.Error()), "ambiguous"):
		http.Error(w, "More than one alarm matched; nothing was changed.", http.StatusConflict)
		return
	}
	// Unexpected manager errors keep details in the log.
	if h.logger != nil {
		h.logger.Error("alert action failed", "action", action, "error", err.Error())
	}
	http.Error(w, "Could not save that alert.", http.StatusInternalServerError)
}

// alertAudio configures pending alerts through their existing managers.
func (h *Handler) alertAudio(w http.ResponseWriter, r *http.Request) {
	if err := limitForm(w, r); err != nil {
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
		if err == nil {
			switch {
			case out.Ambiguous:
				err = errAlarmAmbiguous
			case out.Matched != 1:
				err = errAlarmNotPending
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.alertError(w, "save audio", err)
		return
	}
	h.writeJSON(w, 200, h.buildAlertsView("Completion audio saved.", "", nil))
}

// alertCreate lets the Dashboard schedule a timer or clock alarm with media.
func (h *Handler) alertCreate(w http.ResponseWriter, r *http.Request) {
	if err := limitForm(w, r); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	deps := h.alertDeps()
	audio, label := strings.TrimSpace(r.FormValue("audio")), r.FormValue("label")
	if len(audio) > 2048 {
		http.Error(w, "Audio must be at most 2048 characters", 400)
		return
	}
	if len(label) > 200 {
		http.Error(w, "Label must be at most 200 characters", 400)
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
		h.alertError(w, "create alert", err)
		return
	}
	h.writeJSON(w, 200, h.buildAlertsView("Alert scheduled.", "", nil))
}
