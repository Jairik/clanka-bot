package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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

	HasStore             bool
	PiRecordingAvailable bool
	// RecordingAvailable is false only when the deployment did not wire the
	// store; the page then explains the missing piece instead of a dead button.
	RecordingAvailable bool
	Phrases            []voicePhraseView
	// AddPhraseAction stores a new phrase with no takes (form field
	// "phrase", plus "current" so a refusal keeps the page where it was).
	AddPhraseAction string
	// ModelsKnown is true when the wakeword service answered, so each
	// phrase's Model is "trained" or "missing" rather than "unknown".
	ModelsKnown bool
	ModelNote   string

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

// voicePhraseView is one selectable phrase and what it still lacks.
type voicePhraseView struct {
	Slug     string
	Text     string
	Selected bool
	Builtin  bool
	// Stored is false only for a phrase that exists in the URL alone (an
	// old link); recording into it stores it.
	Stored    bool
	Positives int
	Negatives int
	// Model is "trained", "missing" or "unknown" (see ModelNote).
	Model string
	// Missing lists the gaps in plain words; NeedsAttention is the `!` mark
	// and StatusLabel its accessible label and tooltip.
	Missing        []string
	NeedsAttention bool
	StatusLabel    string
	// CanRemove is an empty custom phrase; RemoveAction is set for every
	// stored custom phrase so the page can explain why it is disabled.
	CanRemove    bool
	RemoveAction string
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
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), slug, "", ""))
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
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), r.PathValue("slug"), flash, ""))
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
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), r.PathValue("slug"), "Deleted the take.", ""))
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
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), r.PathValue("slug"), fmt.Sprintf("Deleted all %d %s takes.", count, kind), ""))
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
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), slug, "Bundle ready: "+bundle.Name, ""))
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

// voiceSamplesAddPhrase stores a new phrase right away, with no takes, so it
// survives a reload. The answer selects it and its flash names the slug the
// owner's words became.
func (h *Handler) voiceSamplesAddPhrase(w http.ResponseWriter, r *http.Request) {
	if !h.voiceSamplesReady(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "Could not read the phrase.", "")
		return
	}
	entered := strings.TrimSpace(r.PostForm.Get("phrase"))
	current := strings.TrimSpace(r.PostForm.Get("current"))
	slug, err := h.voiceSamples.AddPhrase(entered)
	switch {
	case errors.Is(err, wakesample.ErrInvalidPhrase):
		h.voiceSamplesFail(w, r, http.StatusBadRequest, fmt.Sprintf("“%s” is not a valid phrase name: %s.", entered, wakesample.ErrInvalidPhrase), current)
		return
	case errors.Is(err, wakesample.ErrPhraseExists):
		h.voiceSamplesFail(w, r, http.StatusConflict, fmt.Sprintf("“%s” already exists (%s); pick it above.", wakesample.PhraseText(slug), slug), current)
		return
	case err != nil:
		h.voiceSamplesFail(w, r, http.StatusBadRequest, err.Error(), current)
		return
	}
	h.logger.Info("voice sample phrase added")
	flash := fmt.Sprintf("Added “%s”. Its takes and bundles are named %s.", wakesample.PhraseText(slug), slug)
	h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), slug, flash, ""))
}

// voiceSamplesRemovePhrase deletes an empty custom phrase. A phrase with takes
// is refused, never emptied here: Delete all on each tab is the explicit step.
func (h *Handler) voiceSamplesRemovePhrase(w http.ResponseWriter, r *http.Request) {
	if !h.voiceSamplesReady(w, r) {
		return
	}
	slug := r.PathValue("slug")
	err := h.voiceSamples.RemovePhrase(slug)
	switch {
	case err == nil:
		h.logger.Info("voice sample phrase removed")
		h.writeJSON(w, http.StatusOK, h.buildVoiceSamplesView(r.Context(), "", fmt.Sprintf("Removed “%s”.", wakesample.PhraseText(slug)), ""))
	case errors.Is(err, wakesample.ErrPhraseHasTakes):
		h.voiceSamplesFail(w, r, http.StatusConflict, fmt.Sprintf("“%s” still has takes, so it was not removed. Delete them on the Positives and Negatives tabs first.", wakesample.PhraseText(slug)), slug)
	case errors.Is(err, wakesample.ErrPhraseNotFound):
		h.voiceSamplesFail(w, r, http.StatusNotFound, "That phrase is not stored.", "")
	case errors.Is(err, wakesample.ErrBuiltinPhrase):
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "A built-in phrase cannot be removed.", slug)
	case errors.Is(err, wakesample.ErrInvalidPhrase):
		h.voiceSamplesFail(w, r, http.StatusBadRequest, "That phrase name is not valid.", "")
	default:
		h.voiceSamplesFail(w, r, http.StatusInternalServerError, "The phrase could not be removed.", slug)
	}
}

// wakeModelCache keeps the wakeword service's advertised models briefly. The
// batch recorder refreshes the view after every take, and an unreachable
// service would otherwise add its timeout to each one. It caches the same
// describe answer Health reads; it is not a second list of models.
type wakeModelCache struct {
	mu      sync.Mutex
	at      time.Time
	models  map[string]bool
	err     error
	checked bool
}

const (
	wakeModelCacheTTL     = 30 * time.Second
	wakeModelCacheFailTTL = 10 * time.Second
)

// wakeModelsForStatus returns the advertised models, whether they are known,
// and a one-line note on where the answer came from.
func (h *Handler) wakeModelsForStatus(ctx context.Context) (map[string]bool, bool, string) {
	if strings.TrimSpace(h.wakeword.Addr) == "" {
		return nil, false, "The wakeword service is not configured, so trained models cannot be checked."
	}
	cache := &h.wakeModels
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := h.now()
	ttl := wakeModelCacheTTL
	if cache.err != nil {
		ttl = wakeModelCacheFailTTL
	}
	if !cache.checked || now.Sub(cache.at) >= ttl {
		cache.models, cache.err = h.advertisedWakeModels(ctx)
		cache.at, cache.checked = now, true
	}
	if cache.err != nil {
		return nil, false, "The wakeword service did not answer, so trained models are unknown."
	}
	return cache.models, true, "A phrase has a trained model when the wakeword service advertises it, e.g. volume-up as volume_up."
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
	h.writeJSON(w, status, h.buildVoiceSamplesView(r.Context(), slug, "", message))
}

// buildVoiceSamplesView resolves the phrase, takes, prompts and bundles
// from one bounded snapshot.
func (h *Handler) buildVoiceSamplesView(ctx context.Context, slug, flash, errorText string) voiceSamplesView {
	maxTakes := wakesample.MaxTakes
	if h.voiceSamples != nil {
		maxTakes = h.voiceSamples.TakeLimit()
	}
	view := voiceSamplesView{
		Owner: h.owner, Page: "voice-samples", Flash: flash, Error: errorText,
		PiRecordingAvailable: h.sampleRecorder != nil,
		MinTakes:             wakesample.MinTakes, MaxTakes: maxTakes,
		MaxSeconds: fmt.Sprintf("%.0f s", wakesample.MaxTakeSeconds),
		MaxBytes:   fmt.Sprintf("%d KB", wakesample.MaxTakeBytes/1024),
		Prompts:    wakesample.Prompts,
	}
	var statuses []wakesample.PhraseStatus
	if h.voiceSamples != nil {
		view.HasStore = true
		view.RecordingAvailable = true
		view.AddPhraseAction = "/voice-samples/phrases"
		var models map[string]bool
		models, view.ModelsKnown, view.ModelNote = h.wakeModelsForStatus(ctx)
		statuses = h.voiceSamples.Statuses(models, view.ModelsKnown)
	} else {
		for _, phrase := range wakesample.BuiltinPhrases {
			if builtin, ok := wakesample.SlugFor(phrase); ok {
				statuses = append(statuses, wakesample.PhraseStatus{Slug: builtin, Builtin: true})
			}
		}
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = statuses[0].Slug
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
	for _, status := range statuses {
		stored := h.voiceSamples != nil
		option := phraseOption(status, stored)
		if status.Slug == slug {
			option.Selected = true
			selected = true
		}
		view.Phrases = append(view.Phrases, option)
	}
	if !selected && wakesample.ValidSlug(slug) {
		// A phrase only in the URL (an old link) is not stored yet; show it so
		// the selector says where the owner is. Its first kept take stores it.
		status := wakesample.PhraseStatus{Slug: slug, Model: wakesample.ModelUnknown}
		if view.ModelsKnown {
			status.Model = wakesample.ModelMissing
		}
		option := phraseOption(status, false)
		option.Selected = true
		view.Phrases = append(view.Phrases, option)
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

// phraseOption turns one status into the picker's chip.
func phraseOption(status wakesample.PhraseStatus, stored bool) voicePhraseView {
	option := voicePhraseView{
		Slug:      status.Slug,
		Text:      wakesample.PhraseText(status.Slug),
		Builtin:   status.Builtin,
		Stored:    stored || status.Builtin,
		Positives: status.Positives,
		Negatives: status.Negatives,
		Model:     modelLabel(status.Model),
		Missing:   status.Missing(),
	}
	if !stored && !status.Builtin {
		option.Missing = append([]string{"not saved yet"}, option.Missing...)
	}
	option.NeedsAttention = len(option.Missing) > 0
	if option.NeedsAttention {
		option.StatusLabel = "Needs attention: " + strings.Join(option.Missing, ", ")
	} else if status.Model == wakesample.ModelUnknown {
		option.StatusLabel = "Samples complete; trained model unknown"
	} else {
		option.StatusLabel = "Samples complete and a trained model is served"
	}
	if stored && !status.Builtin {
		option.RemoveAction = "/voice-samples/phrases/" + url.PathEscape(status.Slug) + "/remove"
		option.CanRemove = status.Positives == 0 && status.Negatives == 0
	}
	return option
}

func modelLabel(state wakesample.ModelState) string {
	switch state {
	case wakesample.ModelTrained:
		return "trained"
	case wakesample.ModelMissing:
		return "missing"
	default:
		return "unknown"
	}
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
