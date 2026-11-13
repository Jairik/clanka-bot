package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// volumeTimeout bounds one pactl round trip so a wedged Pulse server cannot
// hang the top bar.
const volumeTimeout = 3 * time.Second

// VolumeControl is the Pi speaker's level and mute. main passes the same
// Pulse sink the replies and audio.volume_set use.
type VolumeControl interface {
	Volume(ctx context.Context) (audio.VolumeState, error)
	SetVolume(ctx context.Context, percent int) error
	SetMute(ctx context.Context, muted bool) error
}

// volumeView is the top bar's speaker state. Available is false when no sink
// is wired or the read failed, and the control hides itself.
type volumeView struct {
	Available bool
	Percent   int
	Muted     bool
	Error     string
}

func (h *Handler) volumeGet(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.readVolume(r.Context(), ""))
}

// volumeSet applies a level, a mute flag, or both. Setting a level above zero
// also unmutes, so dragging the slider is always audible.
func (h *Handler) volumeSet(w http.ResponseWriter, r *http.Request) {
	if h.volume == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := r.ParseForm(); err != nil {
		h.writeJSON(w, http.StatusBadRequest, h.readVolume(r.Context(), "Could not read the form."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), volumeTimeout)
	defer cancel()
	if raw := r.FormValue("percent"); raw != "" {
		percent, err := strconv.Atoi(raw)
		if err != nil || percent < 0 || percent > 100 {
			h.writeJSON(w, http.StatusBadRequest, h.readVolume(r.Context(), "Volume must be a whole percent from 0 to 100."))
			return
		}
		if err := h.volume.SetVolume(ctx, percent); err != nil {
			h.volumeFail(w, r, err)
			return
		}
		if percent > 0 && !r.Form.Has("muted") {
			if err := h.volume.SetMute(ctx, false); err != nil {
				h.volumeFail(w, r, err)
				return
			}
		}
	}
	if raw := r.FormValue("muted"); raw != "" {
		muted, err := strconv.ParseBool(raw)
		if err != nil {
			h.writeJSON(w, http.StatusBadRequest, h.readVolume(r.Context(), "Muted must be true or false."))
			return
		}
		if err := h.volume.SetMute(ctx, muted); err != nil {
			h.volumeFail(w, r, err)
			return
		}
	}
	h.writeJSON(w, http.StatusOK, h.readVolume(r.Context(), ""))
}

func (h *Handler) volumeFail(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Warn("dashboard volume", "error", err.Error())
	h.writeJSON(w, http.StatusBadGateway, h.readVolume(r.Context(), "The speaker did not accept that change."))
}

func (h *Handler) readVolume(ctx context.Context, errorText string) volumeView {
	if h.volume == nil {
		return volumeView{Error: errorText}
	}
	ctx, cancel := context.WithTimeout(ctx, volumeTimeout)
	defer cancel()
	state, err := h.volume.Volume(ctx)
	if err != nil {
		h.logger.Warn("dashboard volume read", "error", err.Error())
		return volumeView{Error: "The speaker volume is unavailable."}
	}
	return volumeView{Available: true, Percent: state.Percent, Muted: state.Muted, Error: errorText}
}
