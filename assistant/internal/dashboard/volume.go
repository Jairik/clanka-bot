package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// volumeTimeout bounds one pactl read so a wedged Pulse server cannot hang the
// top bar. Reads now cover two concurrent queries, so this is the budget for
// the whole read rather than half of it.
const volumeTimeout = 3 * time.Second

// VolumeControl is the Pi speaker's level and mute. main passes the same
// Pulse sink the replies and audio.volume_set use.
type VolumeControl interface {
	Volume(ctx context.Context) (audio.VolumeState, error)
	SetVolume(ctx context.Context, percent int) error
	SetMute(ctx context.Context, muted bool) error
}

// volumeCache remembers the last successful read. A Bluetooth sink can take
// seconds to wake, so a failed read serves the last level the user can still
// drag instead of taking the control off the screen; a stale slider beats no
// slider. The zero value is ready to use and each Handler owns its own.
type volumeCache struct {
	mu     sync.Mutex
	state  audio.VolumeState
	loaded bool
}

func (c *volumeCache) get() (audio.VolumeState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.loaded
}

func (c *volumeCache) put(state audio.VolumeState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
	c.loaded = true
}

// volumeView is the top bar's speaker state. Available is false only when no
// sink is wired or a read failed with nothing cached; the control hides then.
// Stale marks a level we could not confirm on this read: it is the last known
// good value, and the client shows its own note instead of an error sentence.
type volumeView struct {
	Available bool
	Stale     bool
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
	// The writes below succeed without the sink answering them, so after one
	// lands we know its part of the state even if the read that answers this
	// request times out. Track it and seed the cache with it (see the end).
	var (
		percent   int
		havePct   bool
		muted     bool
		haveMuted bool
	)
	if raw := r.FormValue("percent"); raw != "" {
		level, err := strconv.Atoi(raw)
		if err != nil || level < 0 || level > 100 {
			h.writeJSON(w, http.StatusBadRequest, h.readVolume(r.Context(), "Volume must be a whole percent from 0 to 100."))
			return
		}
		if err := h.volume.SetVolume(ctx, level); err != nil {
			h.volumeFail(w, r, err)
			return
		}
		percent, havePct = level, true
		if level > 0 && !r.Form.Has("muted") {
			if err := h.volume.SetMute(ctx, false); err != nil {
				h.volumeFail(w, r, err)
				return
			}
			muted, haveMuted = false, true
		}
	}
	if raw := r.FormValue("muted"); raw != "" {
		flag, err := strconv.ParseBool(raw)
		if err != nil {
			h.writeJSON(w, http.StatusBadRequest, h.readVolume(r.Context(), "Muted must be true or false."))
			return
		}
		if err := h.volume.SetMute(ctx, flag); err != nil {
			h.volumeFail(w, r, err)
			return
		}
		muted, haveMuted = flag, true
	}
	// Everything above landed, so the sink is in the state we asked for even if
	// it will not report it. Seed the cache before confirming, otherwise a read
	// that times out answers with the level from *before* the write and the top
	// bar snaps back to it. A partial write (level only, nothing cached and no
	// unmute) does not describe the whole state, so it seeds nothing.
	if prior, ok := h.volumeCache.get(); ok || (havePct && haveMuted) {
		if havePct {
			prior.Percent = percent
		}
		if haveMuted {
			prior.Muted = muted
		}
		h.volumeCache.put(prior)
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
		if cached, ok := h.volumeCache.get(); ok {
			// The sink did not answer, but the last level still works. Keep the
			// control on screen and let the client mark it stale. errorText is
			// a write's refusal and is still the client's message to show.
			return volumeView{Available: true, Stale: true, Percent: cached.Percent, Muted: cached.Muted, Error: errorText}
		}
		return volumeView{Error: "The speaker volume is unavailable."}
	}
	h.volumeCache.put(state)
	return volumeView{Available: true, Percent: state.Percent, Muted: state.Muted, Error: errorText}
}
