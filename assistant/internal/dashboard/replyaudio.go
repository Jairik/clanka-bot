package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
)

// replyAudioFile is the fixed inline download name; the browser player only
// needs a stable extension, not a name derived from the reply.
const replyAudioFile = "reply.wav"

// replyAudio serves one stored answer as WAV for the browser's <audio> element.
// It is an authenticated GET whose path carries only the request id: neither a
// token nor the reply text is ever put in a query string. The reply is resolved
// from the in-process Activity store with the same voice and rate a live reply
// would use (speechVoice), so the active voice setting is honoured without the
// URL carrying it.
//
// A Local-only reply is already visible on this authenticated page. Reading it
// aloud into the owner's own browser therefore does not widen what leaves the
// Pi: the text goes from the Activity store to the TTS service and back to the
// very browser that can already see it. Nothing here plays through the Pi
// speaker, touches the speak-replies opt-in, or changes the active voice.
//
// The id (not the words) is the whole lookup key, so a blank or unknown id and
// a blank or over-long reply are refused before synthesis with a short line
// the exchange can show.
func (h *Handler) replyAudio(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	// The same nil checks as the CanPlayReplies session flag, so the control and
	// the endpoint cannot disagree about whether playback is possible.
	if h.activity == nil || h.voices == nil || h.voicePreview == nil {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "No reply was requested.", http.StatusBadRequest)
		return
	}
	// Get reads the bounded ring, which holds Local-only results too.
	entry, ok := h.activity.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	reply := strings.TrimSpace(entry.Reply)
	if reply == "" {
		http.Error(w, "This reply has no text to play.", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(reply) > voicesample.MaxPreviewRunes {
		http.Error(w, replyAudioTooLong(), http.StatusBadRequest)
		return
	}
	ref, rate := h.speechVoice()
	data, err := h.voicePreview.PreviewText(r.Context(), ref, rate, reply)
	if err != nil {
		switch {
		case errors.Is(err, voicesample.ErrTextEmpty):
			http.Error(w, "This reply has no text to play.", http.StatusBadRequest)
		case errors.Is(err, voicesample.ErrTextTooLong):
			http.Error(w, replyAudioTooLong(), http.StatusBadRequest)
		default:
			h.logger.Warn("reply audio failed", "error", err.Error())
			http.Error(w, "Reply audio unavailable.", http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	// The bytes are the owner's reply in the voice and rate from settings, none
	// of which the URL carries: a cached copy could replay the wrong take.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": replyAudioFile}))
	// ServeContent gives the player the same Range and Content-Length handling
	// as the wording preview and /voices/audio.
	http.ServeContent(w, r, replyAudioFile, time.Time{}, bytes.NewReader(data))
}

// replyAudioTooLong is the one over-long message the exchange shows.
func replyAudioTooLong() string {
	return fmt.Sprintf("Replies longer than %d characters can't be played aloud.", voicesample.MaxPreviewRunes)
}
