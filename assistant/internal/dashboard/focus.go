package dashboard

import (
	"net/http"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
)

// focusView reports what currently holds the user's attention and the
// unprompted-speech queue, without any announcement text (#217). It is the one
// read for the Activity header's "what is happening now" strip and the matching
// SSE delta.
type focusView struct {
	// Ring is true while a ringing Scheduled alert holds Focus.
	Ring bool `json:"ring"`
	// Reply is true while a spoken reply holds Focus.
	Reply bool `json:"reply"`
	// Music is true while music holds Focus.
	Music bool `json:"music"`
	// Announcements reports the queue without any text: how many are waiting,
	// whether a hold (Quiet hours) is on, and whether one is playing now.
	Announcements announceSnapshot `json:"announcements"`
}

// announceSnapshot is the queue state with no message content (#217).
type announceSnapshot struct {
	// Waiting is the number of announcements queued behind the current turn.
	Waiting int `json:"waiting"`
	// Held is true while a hold (Quiet hours) keeps announcements queued.
	Held bool `json:"held"`
	// Playing is true while one announcement is speaking.
	Playing bool `json:"playing"`
}

// focusView builds the current focus and announcement snapshot. Either source
// may be nil (hidden), in which case its fields report the zero state.
func (h *Handler) focusView() focusView {
	v := focusView{}
	if h.focusReg != nil {
		snap := h.focusReg.Snapshot()
		v.Ring = snap[focus.Ring]
		v.Reply = snap[focus.Reply]
		v.Music = snap[focus.Music]
	}
	if h.announceQueue != nil {
		snap := h.announceQueue.Snapshot()
		v.Announcements = announceSnapshot{Waiting: snap.Len, Held: snap.Held, Playing: snap.Playing}
	}
	return v
}

// focusHandler serves the focus view over the authenticated Dashboard API.
func (h *Handler) focusHandler(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.focusView())
}

// wireFocusHooks publishes a "focus" SSE delta whenever a Focus claim is taken
// or released, or an announcement is queued or played. The registry and queue
// already hold their own observers for other concerns; we wrap them so the
// existing callbacks still run and the live stream gets its delta.
func (h *Handler) wireFocusHooks() {
	if h.focusReg != nil {
		reg := h.focusReg
		prevReleased := reg.OnReleased
		reg.OnReleased = func(p focus.Priority) {
			if prevReleased != nil {
				prevReleased(p)
			}
			h.live.publish("focus", h.focusView())
		}
		prevClaimed := reg.OnClaimed
		reg.OnClaimed = func(p focus.Priority) {
			if prevClaimed != nil {
				prevClaimed(p)
			}
			h.live.publish("focus", h.focusView())
		}
	}
	if h.announceQueue != nil {
		q := h.announceQueue
		prev := q.OnChange
		q.OnChange = func() {
			if prev != nil {
				prev()
			}
			h.live.publish("focus", h.focusView())
		}
	}
}
