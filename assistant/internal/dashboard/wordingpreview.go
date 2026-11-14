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

	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
)

// The Settings page's agent-wording preview contract. One fixed authenticated
// GET synthesizes spoken wording; the kind tells the app whether a row has
// audio at all. Both values are assigned server-side per setting, so the app
// never matches setting key strings.
const (
	wordingPreviewAction = "/settings/wording/preview"
	wordingPreviewSpoken = "spoken"
	wordingPreviewMatch  = "match"
)

// wordingPreviewFile is the fixed inline download name; the browser player only
// needs a stable extension, not the voice's own name.
const wordingPreviewFile = "wording-preview.wav"

// speechVoice resolves the voice reference and rate a live reply would use,
// mirroring cmd/assistant's currentSpeechVoice fallback: the persisted active
// voice when it is usable, else the default built-in. A stale or unvalidated
// active voice therefore previews in the default voice instead of failing.
// Nothing here writes the active setting.
func (h *Handler) speechVoice() (string, float64) {
	if h.voices == nil {
		return "", voicelib.DefaultRate
	}
	if ref, rate, ok := h.voices.ActiveSpeech(); ok {
		return ref, rate
	}
	return h.voices.DefaultName(), h.voices.Rate()
}

// speechVoiceName resolves the library name a spoken preview will use, as a
// name rather than a speech ref: a custom voice's ref is a path inside the TTS
// mount, which is not a display label. It applies the same usability rule as
// speechVoice (ActiveSpeech, i.e. Active plus SpeechRef), so the Settings row
// label and the voice the endpoint synthesizes with cannot disagree: a stale
// or unvalidated active voice reports the default built-in, exactly as the
// preview falls back. Nil-safe: an unwired library has no label.
func (h *Handler) speechVoiceName() string {
	if h.voices == nil {
		return ""
	}
	if active, ok := h.voices.Active(); ok {
		if _, err := h.voices.SpeechRef(active.Name); err == nil {
			return active.Name
		}
	}
	return h.voices.DefaultName()
}

// settingsWordingPreview synthesizes the wording being edited in Settings with
// the active voice and serves it as WAV for the browser's <audio> element. It
// is an authenticated GET that returns bytes only: it never plays through the
// Pi speaker and never changes the active voice. The draft is validated here,
// so blank or over-long text is a 400 rather than a TTS failure surfacing as
// 502. The text is read from the query string because a preview is a read.
func (h *Handler) settingsWordingPreview(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	// The same two nil checks as settingsView.CanPreview, so the flag and the
	// endpoint cannot disagree.
	if h.voices == nil || h.voicePreview == nil {
		http.NotFound(w, r)
		return
	}
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	if text == "" {
		http.Error(w, "Type some wording to preview.", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(text) > voicesample.MaxPreviewRunes {
		http.Error(w, wordingPreviewTooLong(), http.StatusBadRequest)
		return
	}
	ref, rate := h.speechVoice()
	data, err := h.voicePreview.PreviewText(r.Context(), ref, rate, text)
	if err != nil {
		// The sampler re-checks the draft; map its sentinels rather than
		// letting a refused input look like a service failure.
		switch {
		case errors.Is(err, voicesample.ErrTextEmpty):
			http.Error(w, "Type some wording to preview.", http.StatusBadRequest)
		case errors.Is(err, voicesample.ErrTextTooLong):
			http.Error(w, wordingPreviewTooLong(), http.StatusBadRequest)
		default:
			h.logger.Warn("wording preview failed", "error", err.Error())
			http.Error(w, "Wording preview unavailable.", http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": wordingPreviewFile}))
	// ServeContent gives the same Range and Content-Length handling as
	// /voices/audio, so the player can scrub and loop.
	http.ServeContent(w, r, wordingPreviewFile, time.Time{}, bytes.NewReader(data))
}

// wordingPreviewTooLong is the one over-long message the page shows.
func wordingPreviewTooLong() string {
	return fmt.Sprintf("Wording previews are limited to %d characters.", voicesample.MaxPreviewRunes)
}
