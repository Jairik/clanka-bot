package dashboard

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wakesample"
)

// voiceSamplesView is the wake-phrase recorder's data model. Counts, prompts,
// action URLs and the copy commands are all resolved in Go, so the page only
// lays them out.
type voiceSamplesView struct {
	Owner string
	Page  string
	Flash string
	Error string

	HasStore bool
	// RecordingAvailable is false only when the deployment did not wire the
	// store; the page then explains the missing piece instead of a dead button.
	RecordingAvailable bool
	Phrases            []voicePhraseView

	Slug   string
	Phrase string

	PositiveCount int
	NegativeCount int
	MinTakes      int
	MaxTakes      int
	MaxSeconds    string
	MaxBytes      string

	Positives []voiceTakeView
	Negatives []voiceTakeView

	Prompts            []string
	NextPrompt         string
	NextNegativePrompt string

	UploadPositive string
	UploadNegative string
	SubmitAction   string
	ClearPositive  string
	ClearNegative  string
	CanSubmit      bool

	Bundles []voiceBundleView
}

// voicePhraseView is one selectable phrase.
type voicePhraseView struct {
	Slug     string
	Text     string
	Selected bool
}

// voiceTakeView is one stored clip and its two actions.
type voiceTakeView struct {
	ID           string
	Duration     string
	Size         string
	Added        string
	AudioURL     string
	DeleteAction string
}

// voiceBundleView is one finished sample set and the owner's copy choices.
type voiceBundleView struct {
	Name        string
	Size        string
	Created     string
	DownloadURL string
	HostPath    string
	SCP         string
	Taildrop    string
	Positives   int
	Negatives   int
	HasCopyPath bool
}

// voiceSamplesPage renders the recorder for one phrase.
func (h *Handler) voiceSamplesPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("phrase"))
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(slug, "", ""))
}

// voiceSamplesUpload accepts one take as a raw WAV body. The size and format
// are checked before anything is stored, so an upload cannot grow the set with
// a clip the training pipeline would reject.
func (h *Handler) voiceSamplesUpload(w http.ResponseWriter, r *http.Request) {
	if !h.voiceSamplesReady(w, r) {
		return
	}
	kind, ok := wakesample.ParseKind(r.PathValue("kind"))
	if !ok {
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "Unknown sample kind.", r.PathValue("slug"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, wakesample.MaxTakeBytes+1)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "request body too large") {
			h.voiceSamplesFail(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("The take is larger than %d KB.", wakesample.MaxTakeBytes/1024), r.PathValue("slug"))
			return
		}
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "Could not read the take.", r.PathValue("slug"))
		return
	}
	if len(data) > wakesample.MaxTakeBytes {
		h.voiceSamplesFail(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("The take is larger than %d KB.", wakesample.MaxTakeBytes/1024), r.PathValue("slug"))
		return
	}
	take, err := h.voiceSamples.Add(r.PathValue("slug"), kind, data)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already has") {
			status = http.StatusConflict
		}
		h.voiceSamplesFail(w, r, status, err.Error(), r.PathValue("slug"))
		return
	}
	// The take id is a sequence number, not audio: logging it tells an operator
	// a retry worked without putting a phrase or a filename in the logs.
	h.logger.Info("voice sample kept", "kind", string(kind), "take", take.ID)
	flash := fmt.Sprintf("Kept take %d. Next: %s", h.voiceSamples.Count(r.PathValue("slug"), kind), wakesample.PromptFor(h.voiceSamples.Count(r.PathValue("slug"), wakesample.Positive)))
	if kind == wakesample.Negative {
		flash = fmt.Sprintf("Kept negative take %d.", h.voiceSamples.Count(r.PathValue("slug"), kind))
	}
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.PathValue("slug"), flash, ""))
}

// voiceSamplesDelete removes one take. A missing id is a no-op, so a double
// tap cannot show an error.
func (h *Handler) voiceSamplesDelete(w http.ResponseWriter, r *http.Request) {
	if !h.voiceSamplesReady(w, r) {
		return
	}
	kind, ok := wakesample.ParseKind(r.PathValue("kind"))
	if !ok {
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "Unknown sample kind.", r.PathValue("slug"))
		return
	}
	if err := h.voiceSamples.Delete(r.PathValue("slug"), kind, r.PathValue("id")); err != nil {
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "That take could not be deleted.", r.PathValue("slug"))
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.PathValue("slug"), "Deleted the take.", ""))
}

// voiceSamplesClear removes every take of one kind.
func (h *Handler) voiceSamplesClear(w http.ResponseWriter, r *http.Request) {
	if !h.voiceSamplesReady(w, r) {
		return
	}
	kind, ok := wakesample.ParseKind(r.PathValue("kind"))
	if !ok {
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "Unknown sample kind.", r.PathValue("slug"))
		return
	}
	count := h.voiceSamples.Count(r.PathValue("slug"), kind)
	if err := h.voiceSamples.Clear(r.PathValue("slug"), kind); err != nil {
		h.voiceSamplesFail(w, r, http.StatusInternalServerError, "The takes could not be deleted.", r.PathValue("slug"))
		return
	}
	h.logger.Info("voice samples cleared", "kind", string(kind), "takes", count)
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.PathValue("slug"), fmt.Sprintf("Deleted all %d %s takes.", count, kind), ""))
}

// voiceSamplesSubmit writes the finished set as a .tar.gz and returns the view
// with the copy commands. It never starts training or uploads anything.
func (h *Handler) voiceSamplesSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.voiceSamplesReady(w, r) {
		return
	}
	slug := r.PathValue("slug")
	bundle, err := h.voiceSamples.Submit(slug)
	if err != nil {
		h.voiceSamplesFail(w, r, http.StatusBadRequest, err.Error(), slug)
		return
	}
	// Counts only: the bundle name is a phrase and timestamp, no audio.
	h.logger.Info("voice sample bundle", "positives", bundle.Positives, "negatives", bundle.Negatives, "bytes", bundle.Bytes)
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(slug, "Bundle ready: "+bundle.Name, ""))
}

// voiceSamplesAudio serves one kept take for playback in the browser.
func (h *Handler) voiceSamplesAudio(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if h.voiceSamples == nil {
		http.NotFound(w, r)
		return
	}
	kind, ok := wakesample.ParseKind(r.PathValue("kind"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	path, err := h.voiceSamples.Path(r.PathValue("slug"), kind, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": id + ".wav"}))
	http.ServeContent(w, r, id+".wav", info.ModTime(), file)
}

// voiceSamplesBundle streams one finished bundle for the Download button.
func (h *Handler) voiceSamplesBundle(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if h.voiceSamples == nil {
		http.NotFound(w, r)
		return
	}
	slug, name := r.PathValue("slug"), r.PathValue("name")
	file, bundle, err := h.voiceSamples.OpenBundle(slug, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, bundle.CreatedAt, file)
}

// voiceSamplesReady guards the write and audio routes against an unwired
// store; the page still renders an explanation.
func (h *Handler) voiceSamplesReady(w http.ResponseWriter, r *http.Request) bool {
	if h.voiceSamples == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (h *Handler) voiceSamplesFail(w http.ResponseWriter, r *http.Request, status int, message, slug string) {
	h.writeJSON(w, status, h.buildVoiceSamplesView(slug, "", message))
}

// buildVoiceSamplesView resolves the phrase, takes, prompts and bundles from
// one bounded snapshot.
func (h *Handler) buildVoiceSamplesView(slug, flash, errorText string) voiceSamplesView {
	maxTakes := wakesample.MaxTakes
	if h.voiceSamples != nil {
		maxTakes = h.voiceSamples.TakeLimit()
	}
	view := voiceSamplesView{
		Owner: h.owner, Page: "voice-samples", Flash: flash, Error: errorText,
		MinTakes: wakesample.MinTakes, MaxTakes: maxTakes,
		MaxSeconds: fmt.Sprintf("%.0f s", wakesample.MaxTakeSeconds),
		MaxBytes:   fmt.Sprintf("%d KB", wakesample.MaxTakeBytes/1024),
		Prompts:    wakesample.Prompts,
	}
	phrases := wakesample.BuiltinPhrases
	if h.voiceSamples != nil {
		view.HasStore = true
		view.RecordingAvailable = true
		phrases = nil
		for _, stored := range h.voiceSamples.Phrases() {
			phrases = append(phrases, wakesample.PhraseText(stored))
		}
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug, _ = wakesample.SlugFor(phrases[0])
	} else if !wakesample.ValidSlug(slug) {
		if errorText == "" {
			errorText = "That phrase name is not valid. Use letters, digits and single dashes."
			view.Error = errorText
		}
		slug = wakesample.BuiltinPhrases[0]
	}
	view.Slug = slug
	view.Phrase = wakesample.PhraseText(slug)
	selected := false
	for _, phrase := range phrases {
		phraseSlug, ok := wakesample.SlugFor(phrase)
		if !ok {
			continue
		}
		option := voicePhraseView{Slug: phraseSlug, Text: wakesample.PhraseText(phraseSlug)}
		if phraseSlug == slug {
			option.Selected = true
			selected = true
		}
		view.Phrases = append(view.Phrases, option)
	}
	if !selected && wakesample.ValidSlug(slug) {
		// A new phrase is not stored yet, so it is not in Phrases; add it so
		// the selector shows where the owner is.
		view.Phrases = append(view.Phrases, voicePhraseView{Slug: slug, Text: view.Phrase, Selected: true})
	}
	if h.voiceSamples == nil {
		return view
	}

	view.PositiveCount = h.voiceSamples.Count(slug, wakesample.Positive)
	view.NegativeCount = h.voiceSamples.Count(slug, wakesample.Negative)
	view.NextPrompt = wakesample.PromptFor(view.PositiveCount)
	view.NextNegativePrompt = wakesample.NegativePromptFor(view.NegativeCount)
	view.UploadPositive = "/voice-samples/takes/" + url.PathEscape(slug) + "/positive"
	view.UploadNegative = "/voice-samples/takes/" + url.PathEscape(slug) + "/negative"
	view.SubmitAction = "/voice-samples/submit/" + url.PathEscape(slug)
	view.ClearPositive = "/voice-samples/clear/" + url.PathEscape(slug) + "/positive"
	view.ClearNegative = "/voice-samples/clear/" + url.PathEscape(slug) + "/negative"
	view.CanSubmit = view.PositiveCount >= wakesample.MinTakes && view.PositiveCount <= view.MaxTakes

	for _, group := range []struct {
		kind  wakesample.Kind
		views *[]voiceTakeView
	}{{wakesample.Positive, &view.Positives}, {wakesample.Negative, &view.Negatives}} {
		takes, err := h.voiceSamples.Takes(slug, group.kind)
		if err != nil {
			continue
		}
		for _, take := range takes {
			*group.views = append(*group.views, voiceTakeView{
				ID:           take.ID,
				Duration:     take.Duration.Round(10 * time.Millisecond).String(),
				Size:         humanBytes(take.Bytes),
				Added:        take.AddedAt.Local().Format("2 Jan 15:04"),
				AudioURL:     wakesample.AudioURL(slug, take.Kind, take.ID),
				DeleteAction: "/voice-samples/takes/" + url.PathEscape(slug) + "/" + url.PathEscape(string(take.Kind)) + "/" + url.PathEscape(take.ID) + "/delete",
			})
		}
	}

	bundles, err := h.voiceSamples.Bundles(slug, 3)
	if err == nil {
		for _, bundle := range bundles {
			view.Bundles = append(view.Bundles, voiceBundleView{
				Name:        bundle.Name,
				Size:        humanBytes(bundle.Bytes),
				Created:     bundle.CreatedAt.Local().Format("2 Jan 15:04"),
				DownloadURL: wakesample.DownloadURL(slug, bundle.Name),
				HostPath:    bundle.HostPath,
				SCP:         bundle.SCP,
				Taildrop:    bundle.Taildrop,
				Positives:   bundle.Positives,
				Negatives:   bundle.Negatives,
				HasCopyPath: bundle.HostPath != "",
			})
		}
	}
	return view
}

// humanBytes keeps a take or bundle size readable without a units package.
func humanBytes(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}
