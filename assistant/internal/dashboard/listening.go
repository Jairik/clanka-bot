package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voice"
)

// listeningView is the top bar's wake-word listening state.
type listeningView struct {
	Available bool
	Listening bool
	Until     string
	Error     string
}

// listeningRead computes the effective listening state for the dashboard.
func (h *Handler) listeningRead() listeningView {
	if strings.TrimSpace(h.wakeword.Addr) == "" || h.settings == nil {
		return listeningView{Available: false}
	}
	active, until := voice.ListeningState(h.settings, h.now())
	return listeningView{
		Available: true,
		Listening: active,
		Until:     until,
	}
}

// listeningGet returns the current effective listening state.
func (h *Handler) listeningGet(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.listeningRead())
}

// listeningSet writes the listening and listening_until pair in a single batch write.
func (h *Handler) listeningSet(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(h.wakeword.Addr) == "" || h.settings == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
	if err := r.ParseForm(); err != nil {
		view := h.listeningRead()
		view.Error = "Could not read the form."
		h.writeJSON(w, http.StatusBadRequest, view)
		return
	}

	var (
		listening bool
		until     string
	)

	// Check if both listening and listening_until are explicitly provided.
	if r.Form.Has("listening") && r.Form.Has("listening_until") {
		listenVal, err := strconv.ParseBool(r.FormValue("listening"))
		if err != nil {
			view := h.listeningRead()
			view.Error = "listening must be true or false."
			h.writeJSON(w, http.StatusBadRequest, view)
			return
		}
		untilVal := strings.TrimSpace(r.FormValue("listening_until"))
		if untilVal != "" {
			if _, err := time.Parse(time.RFC3339, untilVal); err != nil {
				view := h.listeningRead()
				view.Error = "listening_until must be an RFC3339 time."
				h.writeJSON(w, http.StatusBadRequest, view)
				return
			}
		}
		listening = listenVal
		until = untilVal
	} else if durStr := r.FormValue("duration"); durStr != "" {
		dur, err := parseListeningDuration(durStr)
		if err != nil {
			view := h.listeningRead()
			view.Error = err.Error()
			h.writeJSON(w, http.StatusBadRequest, view)
			return
		}
		listening = true
		until = h.now().Add(dur).Format(time.RFC3339)
	} else if minStr := r.FormValue("minutes"); minStr != "" {
		dur, err := parseListeningDuration(minStr)
		if err != nil {
			view := h.listeningRead()
			view.Error = err.Error()
			h.writeJSON(w, http.StatusBadRequest, view)
			return
		}
		listening = true
		until = h.now().Add(dur).Format(time.RFC3339)
	} else {
		raw := r.FormValue("listening")
		if raw == "" {
			raw = r.FormValue("value")
		}
		if raw == "" {
			if r.FormValue("resume") == "true" {
				raw = "true"
			} else if r.FormValue("pause") == "true" {
				raw = "false"
			}
		}
		flag, err := strconv.ParseBool(raw)
		if err != nil {
			view := h.listeningRead()
			view.Error = "Send listening=true to resume or listening=false to pause."
			h.writeJSON(w, http.StatusBadRequest, view)
			return
		}
		listening = flag
		until = ""
	}

	values := map[string]any{
		voice.ListeningKey:      listening,
		voice.ListeningUntilKey: until,
	}
	if err := h.settings.SetMany(r.Context(), values); err != nil {
		h.logger.Warn("dashboard listening set", "error", err.Error())
		view := h.listeningRead()
		view.Error = err.Error()
		h.writeJSON(w, http.StatusBadRequest, view)
		return
	}
	h.writeJSON(w, http.StatusOK, h.listeningRead())
}

// parseListeningDuration parses duration strings like "15m", "30m", "60m" or plain minutes like "15".
func parseListeningDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d, nil
	}
	if minutes, err := strconv.Atoi(raw); err == nil && minutes > 0 {
		return time.Duration(minutes) * time.Minute, nil
	}
	return 0, fmt.Errorf("duration must be a positive duration such as 15m or 30")
}
