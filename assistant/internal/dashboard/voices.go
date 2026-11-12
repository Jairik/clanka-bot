package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
)

// VoicePreview is the TTS boundary the Voice page uses for one bounded browser
// sample. voicesample.Sampler is the production implementation; tests can use
// a fake without opening the audio device.
type VoicePreview interface {
	Preview(ctx context.Context, voiceRef string, rate float64) ([]byte, error)
}

// voicesView is the Voice page's data model. Readiness, status text and every
// URL are precomputed so the template has no conditionals beyond simple flags.
type voicesView struct {
	Owner      string
	Page       string
	Flash      string
	Error      string
	HasLibrary bool
	CanUpload  bool
	CanPreview bool
	MaxUpload  string
	Revision   string
	ActiveName string
	Default    string
	Rate       string
	RateSource string
	RateLabel  string
	RateLower  string
	RateEdit   string
	HasRate    bool
	Voices     []voiceRowView
}

// voiceRowView is one built-in or custom voice and its actions.
type voiceRowView struct {
	Name         string
	Anchor       string
	Origin       string
	Status       string
	StatusClass  string
	Revision     string
	Validated    string
	Added        string
	Note         string
	LastError    string
	StaleNotice  string
	Builtin      bool
	Ready        bool
	Stale        bool
	Active       bool
	Unvalidated  bool
	PreviewURL   string
	SelectAction string
	NoteAction   string
}

// voicesPage renders the library, the effective active voice/rate and each
// row's note form. It is a read-only page when the library or TTS is absent.
func (h *Handler) voicesPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.render(w, "voices_page", h.buildVoicesView(voicesFlashMessage(r.URL.Query().Get("flash")), "", nil))
}

// voicesUpload stages one .safetensors state, runs the header gate and the
// model smoke test through the library, and only then publishes it. A rejected
// extension, size or header never reaches TTS.
func (h *Handler) voicesUpload(w http.ResponseWriter, r *http.Request) {
	if !h.voicesReady(w, r) {
		return
	}
	if h.voiceProbe == nil {
		h.voicesFail(w, r, http.StatusServiceUnavailable, "The speech service is not configured, so an upload cannot be smoke-tested.", nil)
		return
	}
	// The file part is bounded before multipart parsing; the library enforces
	// the same limit while copying so a lying Content-Length cannot exceed it.
	r.Body = http.MaxBytesReader(w, r.Body, voicelib.MaxStateBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "request body too large") {
			h.voicesFail(w, r, http.StatusRequestEntityTooLarge, "The voice state is larger than the 32 MB limit.", nil)
			return
		}
		h.voicesFail(w, r, http.StatusBadRequest, "Could not read the upload form.", nil)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		h.voicesFail(w, r, http.StatusBadRequest, "Choose a .safetensors voice state to upload.", nil)
		return
	}
	defer file.Close()
	if header.Size > voicelib.MaxStateBytes {
		h.voicesFail(w, r, http.StatusRequestEntityTooLarge, "The voice state is larger than the 32 MB limit.", nil)
		return
	}

	replace := strings.TrimSpace(r.FormValue("replace")) != ""
	entry, err := h.voices.Import(r.Context(), file, header.Filename, voicelib.ImportOptions{
		Probe:   h.voiceProbe,
		Replace: replace,
	})
	if err != nil {
		status, message := voiceImportError(err)
		if status >= 500 {
			h.logger.Error("voice upload failed",
				"file", header.Filename, "replace", replace, "error", err.Error())
		} else {
			h.logger.Warn("voice upload rejected",
				"file", header.Filename, "replace", replace, "error", err.Error())
		}
		h.voicesFail(w, r, status, message, nil)
		return
	}
	h.logger.Info("voice uploaded", "name", entry.Name, "revision", entry.ValidatedRevision, "replace", replace)
	h.voicesSuccess(w, r, flashVoiceUploaded)
}

// voicesActive persists one ready entry as the shared voice.name setting. The
// library refuses stale and unvalidated states, so a crafted form cannot
// select a file that never passed the model smoke test.
func (h *Handler) voicesActive(w http.ResponseWriter, r *http.Request) {
	if !h.voicesReady(w, r) {
		return
	}
	name := r.PathValue("name")
	if _, err := h.voices.SetActive(r.Context(), name); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, voicelib.ErrUnknownVoice) {
			status = http.StatusNotFound
		}
		h.voicesFail(w, r, status, err.Error(), nil)
		return
	}
	h.voicesSuccess(w, r, flashVoiceSelected)
}

// voicesRate stores the audible speech rate through the same live settings
// service the wording manager observes, so the cached Filler is re-synthesized
// in the background. Out-of-range values are rejected rather than clamped; a
// selection that silently becomes 2.0 would be surprising.
func (h *Handler) voicesRate(w http.ResponseWriter, r *http.Request) {
	if !h.voicesReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.voicesFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	text := strings.TrimSpace(r.FormValue("value"))
	rate, err := strconv.ParseFloat(text, 64)
	if err != nil || rate < voicelib.MinRate || rate > voicelib.MaxRate {
		h.voicesFail(w, r, http.StatusBadRequest,
			fmt.Sprintf("Rate must be a number between %.1f and %.1f.", voicelib.MinRate, voicelib.MaxRate),
			&editState{Key: voicelib.KeyRate, Value: text})
		return
	}
	if _, err := h.voices.SetRate(r.Context(), rate); err != nil {
		h.voicesFail(w, r, http.StatusBadRequest, err.Error(), &editState{Key: voicelib.KeyRate, Value: text})
		return
	}
	h.voicesSuccess(w, r, flashVoiceRate)
}

// voicesRateReset removes the rate override, revealing config.yaml or the code
// default through the same settings service.
func (h *Handler) voicesRateReset(w http.ResponseWriter, r *http.Request) {
	if !h.voicesReady(w, r) {
		return
	}
	if _, err := h.settings.Reset(r.Context(), voicelib.KeyRate); err != nil {
		h.voicesFail(w, r, settingsErrorStatus(err), err.Error(), nil)
		return
	}
	h.voicesSuccess(w, r, flashReset)
}

// voicesNote records the optional free-text source/consent note. It is stored
// verbatim and shown escaped; nothing enforces or verifies the claim.
func (h *Handler) voicesNote(w http.ResponseWriter, r *http.Request) {
	if !h.voicesReady(w, r) {
		return
	}
	name := r.PathValue("name")
	voice, ok := h.voices.Get(name)
	if !ok {
		h.voicesFail(w, r, http.StatusNotFound, "Unknown voice.", nil)
		return
	}
	if voice.Builtin {
		h.voicesFail(w, r, http.StatusBadRequest, "Built-in voices have no uploaded source note.", nil)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.voicesFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if runes := []rune(note); len(runes) > voicelib.MaxNoteRunes {
		h.voicesFail(w, r, http.StatusBadRequest,
			fmt.Sprintf("The note is limited to %d characters.", voicelib.MaxNoteRunes), nil)
		return
	}
	if err := h.voices.SetNote(r.Context(), voice.Name, note); err != nil {
		h.voicesFail(w, r, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.voicesSuccess(w, r, flashVoiceNote)
}

// voicesAudio synthesizes a fixed short phrase in one library voice and serves
// it as WAV for the browser's <audio> element. It is authenticated by
// ServeHTTP, refuses stale/unvalidated states, and never writes voice.name or
// plays through the Pi speaker.
func (h *Handler) voicesAudio(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if h.voices == nil || h.voicePreview == nil {
		http.NotFound(w, r)
		return
	}
	voice, ok := h.voices.Get(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !voice.Ready() {
		http.Error(w, "voice is not ready for preview", http.StatusConflict)
		return
	}
	ref, err := h.voices.SpeechRef(voice.Name)
	if err != nil {
		http.Error(w, "voice is not ready for preview", http.StatusConflict)
		return
	}
	data, err := h.voicePreview.Preview(r.Context(), ref, h.voices.Rate())
	if err != nil {
		h.logger.Warn("voice preview failed", "voice", voice.Name, "error", err.Error())
		http.Error(w, "voice preview unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": voice.Name + ".wav"}))
	http.ServeContent(w, r, voice.Name+".wav", time.Time{}, bytes.NewReader(data))
}

// voicesReady guards the page and every form route against an unwired service.
// The page itself renders read-only when the library is absent; a write is 404.
func (h *Handler) voicesReady(w http.ResponseWriter, r *http.Request) bool {
	if h.voices == nil || h.settings == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

// voicesSuccess answers htmx with the swapped content; a plain form gets the
// 303 redirect, so both journeys work without JavaScript.
func (h *Handler) voicesSuccess(w http.ResponseWriter, r *http.Request, flash string) {
	if isHTMX(r) {
		h.render(w, "voices_content", h.buildVoicesView(voicesFlashMessage(flash), "", nil))
		return
	}
	http.Redirect(w, r, "/voices?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}

// voicesFail re-renders the page with an error and, when supplied, the rejected
// rate input. htmx only swaps 2xx responses, so a fragment error stays 200.
func (h *Handler) voicesFail(w http.ResponseWriter, r *http.Request, status int, message string, edit *editState) {
	view := h.buildVoicesView("", message, edit)
	if isHTMX(r) {
		h.render(w, "voices_content", view)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	h.render(w, "voices_page", view)
}

// buildVoicesView resolves the library, the effective setting values and every
// per-row URL from one snapshot.
func (h *Handler) buildVoicesView(flash, errorText string, edit *editState) voicesView {
	view := voicesView{
		Owner: h.owner, Page: "voices", Flash: flash, Error: errorText,
		MaxUpload: fmt.Sprintf("%d MB", voicelib.MaxStateBytes/(1<<20)),
	}
	if h.voices == nil {
		return view
	}
	view.HasLibrary = true
	view.CanUpload = h.voiceProbe != nil
	view.CanPreview = h.voicePreview != nil
	view.Revision = h.voices.Revision()
	view.Default = h.voices.DefaultName()

	active, activeOK := h.voices.Active()
	if activeOK {
		view.ActiveName = active.Name
	}

	if h.settings != nil {
		if value, ok := h.settings.Get(voicelib.KeyRate); ok {
			view.HasRate = true
			view.Rate = strconv.FormatFloat(h.voices.Rate(), 'g', -1, 64)
			view.RateEdit = view.Rate
			view.RateSource = string(value.Source)
			view.RateLabel = sourceLabel(value.Source)
			view.RateLower = lowerLayerLabel(value)
			if edit != nil && edit.Key == value.Key {
				view.RateEdit = edit.Value
			}
		}
	}

	for index, voice := range h.voices.List() {
		row := voiceRowView{
			Name:         voice.Name,
			Anchor:       "voice-" + strconv.Itoa(index),
			Builtin:      voice.Builtin,
			Ready:        voice.Ready(),
			Stale:        voice.Stale,
			Unvalidated:  !voice.Builtin && strings.TrimSpace(voice.ValidatedRevision) == "",
			Active:       activeOK && strings.EqualFold(active.Name, voice.Name),
			Note:         voice.Note,
			LastError:    voice.LastError,
			PreviewURL:   "/voices/audio/" + url.PathEscape(voice.Name),
			SelectAction: "/voices/active/" + url.PathEscape(voice.Name),
			NoteAction:   "/voices/note/" + url.PathEscape(voice.Name),
			Origin:       "Custom",
			Status:       "Ready",
			StatusClass:  "pill-ok",
			Revision:     strings.TrimSpace(voice.ValidatedRevision),
		}
		if voice.Builtin {
			row.Origin = "Built-in"
			row.Status = "Built-in"
			row.Revision = view.Revision
			if row.Revision == "" {
				row.Revision = "current"
			}
		} else if voice.Stale {
			row.Status = "Stale"
			row.StatusClass = "pill-timed-out"
			current := view.Revision
			if current == "" {
				current = "a different model revision"
			}
			row.StaleNotice = fmt.Sprintf(
				"This state passed against %s, but the speech model now runs %s. Re-export it on the desktop and upload the new file; it cannot be selected or previewed until then.",
				displayRevision(voice.ValidatedRevision), current)
		} else if row.Unvalidated {
			row.Status = "Unvalidated"
			row.StatusClass = "pill-error"
			row.StaleNotice = "This state has not passed the model smoke test. Re-upload it to validate, or check that the speech service is running."
		}
		if row.Revision == "" {
			row.Revision = "not validated"
		}
		if !voice.AddedAt.IsZero() {
			row.Added = voice.AddedAt.Local().Format("2 Jan 2006")
		}
		if !voice.ValidatedAt.IsZero() {
			row.Validated = voice.ValidatedAt.Local().Format("2 Jan 2006 15:04")
		}
		view.Voices = append(view.Voices, row)
	}
	return view
}

// displayRevision keeps a missing revision readable in the stale notice.
func displayRevision(revision string) string {
	if strings.TrimSpace(revision) == "" {
		return "an unrecorded model revision"
	}
	return revision
}

// voiceImportError maps library errors to a status and a short page message.
// Model and filesystem detail stays in the log; a malformed header is safe to
// show because it names tensors, not host paths.
func voiceImportError(err error) (int, string) {
	switch {
	case errors.Is(err, voicelib.ErrStateTooLarge):
		return http.StatusRequestEntityTooLarge, "The voice state is larger than the 32 MB limit."
	case errors.Is(err, voicelib.ErrUnsupportedState):
		return http.StatusBadRequest, "Only .safetensors voice states are accepted. .pt and .pth pickle files are refused."
	case errors.Is(err, voicelib.ErrInvalidState):
		detail := strings.TrimPrefix(err.Error(), voicelib.ErrInvalidState.Error()+": ")
		detail = strings.TrimPrefix(detail, "voice state: ")
		return http.StatusBadRequest, "The file is not a valid Pocket TTS voice state: " + detail
	case errors.Is(err, voicelib.ErrBuiltinName):
		return http.StatusConflict, "A built-in voice already uses that name. Choose another filename."
	case errors.Is(err, voicelib.ErrVoiceExists):
		if strings.Contains(err.Error(), "capitalisation") {
			return http.StatusConflict, "A custom voice already uses that name with different capitalisation. Rename the file instead."
		}
		return http.StatusConflict, "A custom voice with that name already exists. Tick Replace to re-validate and replace it."
	case errors.Is(err, voicelib.ErrReplaceUnsupported), errors.Is(err, voicelib.ErrStagingUnsupported):
		return http.StatusConflict, "This speech service cannot safely stage or replace a cached voice. Restart both services and try again."
	case errors.Is(err, voicelib.ErrValidationUnavailable):
		return http.StatusServiceUnavailable, "The speech service is not configured, so the state cannot be smoke-tested."
	case errors.Is(err, voicelib.ErrSmokeFailed):
		return http.StatusBadRequest, "The state passed the header check but the model could not use it. Nothing was published."
	default:
		return http.StatusInternalServerError, "The upload failed and was rolled back. Check the assistant log for the reason."
	}
}

func voicesFlashMessage(flash string) string {
	switch flash {
	case flashVoiceUploaded:
		return "Uploaded. The state passed the model smoke test and is ready to select."
	case flashVoiceSelected:
		return "Selected. Replies, Fillers and alerts use this voice from the next synthesis."
	case flashVoiceRate:
		return "Saved. The rate applies to the next reply and the cached Agent Filler is re-synthesized in the background."
	case flashVoiceNote:
		return "Note saved. It is recorded only; nothing enforces or verifies it."
	case flashReset:
		return "Reset. The config.yaml or code-default value applies again."
	default:
		return ""
	}
}

const (
	flashVoiceUploaded = "voice-uploaded"
	flashVoiceSelected = "voice-selected"
	flashVoiceRate     = "voice-rate"
	flashVoiceNote     = "voice-note"
)
