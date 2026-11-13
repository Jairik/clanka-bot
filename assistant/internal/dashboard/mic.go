package dashboard

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

// The browser records the mic, resamples it and uploads the whole utterance
// as one body of raw PCM in exactly this format, so the server never decodes a
// container or a codec.
const (
	micSampleRate = 16000
	micSampleBits = 16
	// micMaxSeconds bounds one recording. A spoken request is a few seconds;
	// the cap keeps a forgotten recording from tying up STT.
	micMaxSeconds = 30
	micMaxBytes   = micSampleRate * micSampleBits / 8 * micMaxSeconds
	// micMinBytes rejects an accidental tap (under a quarter second).
	micMinBytes = micSampleRate * micSampleBits / 8 / 4
	// micTranscribeTimeout bounds the STT call on top of the request context.
	micTranscribeTimeout = 20 * time.Second
)

// Transcriber turns one recorded utterance into text. pcm is 16 kHz mono
// signed 16-bit little-endian. main wires the same Wyoming STT service the
// voice loop uses.
type Transcriber interface {
	Transcribe(ctx context.Context, pcm []byte) (string, error)
}

// activityAskAudio transcribes a browser mic recording and answers it like a
// typed request. The transcript is shown in Activity like any voice request;
// the audio itself is never stored.
func (h *Handler) activityAskAudio(w http.ResponseWriter, r *http.Request) {
	if h.typed == nil || h.transcriber == nil {
		http.NotFound(w, r)
		return
	}
	if !isPCM16k(r.Header.Get("Content-Type")) {
		h.activityFail(w, r, http.StatusUnsupportedMediaType, "The recording must be 16 kHz mono 16-bit PCM.", "")
		return
	}
	pcm, err := io.ReadAll(http.MaxBytesReader(w, r.Body, micMaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.activityFail(w, r, http.StatusRequestEntityTooLarge, "Keep a recording under 30 seconds.", "")
			return
		}
		h.activityFail(w, r, http.StatusBadRequest, "Could not read the recording.", "")
		return
	}
	if len(pcm) < micMinBytes || len(pcm)%2 != 0 {
		h.activityFail(w, r, http.StatusBadRequest, "That recording was too short. Hold the mic and speak.", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), micTranscribeTimeout)
	started := time.Now()
	text, err := h.transcriber.Transcribe(ctx, pcm)
	sttTime := time.Since(started)
	cancel()
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		h.logger.Warn("dashboard mic transcribe", "error", err.Error())
		h.activityFail(w, r, http.StatusBadGateway, "Speech recognition is unavailable right now. Try typing instead.", "")
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		h.activityFail(w, r, http.StatusUnprocessableEntity, "I didn't catch any words in that recording.", "")
		return
	}
	if len([]rune(text)) > maxTypedTextRunes {
		text = string([]rune(text)[:maxTypedTextRunes])
	}
	h.runAsk(w, r, text, latency.SourceDashboardMic, map[latency.Stage]time.Duration{
		latency.StageSTTFinalize: sttTime,
	})
}

// isPCM16k accepts audio/L16 at 16 kHz mono, the RFC 2586 name for raw
// big-endian PCM, which browsers have no other name for. The client sends
// little-endian samples and says so with the endianness parameter.
func isPCM16k(contentType string) bool {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "audio/l16" {
		return false
	}
	if params["rate"] != "16000" {
		return false
	}
	if ch := params["channels"]; ch != "" && ch != "1" {
		return false
	}
	return params["endianness"] == "little-endian"
}
