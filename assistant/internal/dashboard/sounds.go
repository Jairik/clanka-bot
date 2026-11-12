package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
)

// PreviewQueue schedules one Pi preview through the same between-turns gates
// as an Announcement. announce.Queue satisfies it.
type PreviewQueue interface {
	EnqueuePlay(play func(context.Context) error) bool
}

// soundsView is the Sounds page's data model. Everything is precomputed so the
// template contains no lookups or comparisons.
type soundsView struct {
	Owner      string
	Page       string
	Flash      string
	Error      string
	HasLibrary bool
	MaxUpload  string
	Sounds     []soundRowView
	Roles      []soundRoleView
	HasRoles   bool
	RingKinds  []ringKindView
	HasRing    bool
}

// soundRowView is one library entry with both preview routes.
type soundRowView struct {
	Name     string
	Duration string
	Added    string
	Origin   string
	AudioURL string
}

// soundRoleView is one fixed Cue role and its effective assignment.
type soundRoleView struct {
	Role        string
	Label       string
	Key         string
	Cap         string
	Display     string
	Source      string
	SourceLabel string
	LowerLabel  string
	Override    bool
	Options     []soundOption
}

type soundOption struct {
	Value    string
	Label    string
	Selected bool
}

// ringKindView groups the per-kind Ring settings discovered from the settings
// service, so #52's definitions appear without a page change.
type ringKindView struct {
	Kind     string
	Label    string
	Settings []settingView
}

// soundsPage renders the library, role assignments and Ring settings.
func (h *Handler) soundsPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.render(w, "sounds_page", h.buildSoundsView(soundsFlashMessage(r.URL.Query().Get("flash")), "", nil))
}

// soundsUpload accepts one multipart WAV or MP3. The request body and the file
// part are both bounded before decoding; a valid upload never has a role cap
// applied here.
func (h *Handler) soundsUpload(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, audio.MaxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "request body too large") {
			h.soundsFail(w, r, http.StatusRequestEntityTooLarge, "The upload is larger than the 10 MB limit.", nil)
			return
		}
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the upload form.", nil)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, "Choose a WAV or MP3 file to upload.", nil)
		return
	}
	defer file.Close()
	if header.Size > audio.MaxUploadBytes {
		h.soundsFail(w, r, http.StatusRequestEntityTooLarge, "The upload is larger than the 10 MB limit.", nil)
		return
	}
	entry, err := h.sounds.Import(r.Context(), file, header.Filename)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sound.ErrUploadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.soundsFail(w, r, status, err.Error(), nil)
		return
	}
	h.logger.Info("sound uploaded", "name", entry.Name, "duration", entry.Duration.String())
	h.soundsSuccess(w, r, flashUploaded)
}

// soundsRoleSave assigns one library sound to a fixed Cue role through the same
// live setting the Cue reads. The role's definition validator enforces its
// length cap, so a rejected assignment cannot trim or replace the file.
func (h *Handler) soundsRoleSave(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	role := sound.Role(r.PathValue("role"))
	if !sound.Valid(role) {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown Cue role.", nil)
		return
	}
	key := sound.Key(role)
	if _, ok := h.settings.Get(key); !ok {
		h.soundsFail(w, r, http.StatusNotFound, "This Cue role is not configured.", nil)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	value := strings.TrimSpace(r.FormValue("value"))
	if value == "" {
		h.soundsFail(w, r, http.StatusBadRequest, "Choose a sound for this Cue.", nil)
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the sound name.", nil)
		return
	}
	if _, err := h.settings.SetJSON(r.Context(), key, raw); err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.soundsSuccess(w, r, flashSaved)
}

// soundsRoleReset removes one role's override, revealing config.yaml or the
// code default. The library files are untouched.
func (h *Handler) soundsRoleReset(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	role := sound.Role(r.PathValue("role"))
	if !sound.Valid(role) {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown Cue role.", nil)
		return
	}
	key := sound.Key(role)
	if _, ok := h.settings.Get(key); !ok {
		h.soundsFail(w, r, http.StatusNotFound, "This Cue role is not configured.", nil)
		return
	}
	if _, err := h.settings.Reset(r.Context(), key); err != nil {
		h.soundsFail(w, r, settingsErrorStatus(err), err.Error(), nil)
		return
	}
	h.soundsSuccess(w, r, flashReset)
}

// soundsValueSave writes one per-kind Ring setting through the ordinary
// settings service. #52 owns the definitions; this page only edits what the
// service exposes.
func (h *Handler) soundsValueSave(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	key := r.PathValue("key")
	if !isRingSettingKey(key) {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown Ring setting.", nil)
		return
	}
	value, ok := h.settings.Get(key)
	if !ok {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown Ring setting.", nil)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	raw, edit, err := settingFormValue(value, r.FormValue("value"))
	if err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, err.Error(), edit)
		return
	}
	if _, err := h.settings.SetJSON(r.Context(), key, raw); err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, err.Error(), edit)
		return
	}
	flash := flashSaved
	if value.Kind == settings.RestartRequired {
		flash = flashRestart
	}
	h.soundsSuccess(w, r, flash)
}

// soundsValueReset removes one Ring setting's override.
func (h *Handler) soundsValueReset(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	key := r.PathValue("key")
	if !isRingSettingKey(key) {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown Ring setting.", nil)
		return
	}
	value, ok := h.settings.Get(key)
	if !ok {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown Ring setting.", nil)
		return
	}
	if _, err := h.settings.Reset(r.Context(), key); err != nil {
		h.soundsFail(w, r, settingsErrorStatus(err), err.Error(), nil)
		return
	}
	flash := flashReset
	if value.Kind == settings.RestartRequired {
		flash = flashRestart
	}
	h.soundsSuccess(w, r, flash)
}

// soundsPreview queues one library sound for the Pi speaker. With a queue the
// item waits for the current reply, a pending Confirmation and a ringing
// alert; TurnStarted cancels it on Barge-in. A text-only setup has no queue and
// plays synchronously.
func (h *Handler) soundsPreview(w http.ResponseWriter, r *http.Request) {
	if !h.soundsReady(w, r) {
		return
	}
	name := r.PathValue("name")
	if _, ok := h.sounds.Find(name); !ok {
		h.soundsFail(w, r, http.StatusNotFound, "Unknown sound.", nil)
		return
	}
	if h.previews != nil {
		if !h.previews.EnqueuePlay(func(ctx context.Context) error {
			return h.sounds.PlayName(ctx, name)
		}) {
			h.soundsFail(w, r, http.StatusServiceUnavailable, "The Pi preview queue is not available.", nil)
			return
		}
		h.soundsSuccess(w, r, flashQueued)
		return
	}
	if err := h.sounds.PlayName(r.Context(), name); err != nil {
		h.soundsFail(w, r, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.soundsSuccess(w, r, flashPlayed)
}

// soundsAudio serves the canonical WAV for a browser <audio> element. It is an
// authenticated GET behind ServeHTTP, supports Range through ServeContent, and
// only resolves indexed library names (never arbitrary volume paths).
func (h *Handler) soundsAudio(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if h.sounds == nil {
		http.NotFound(w, r)
		return
	}
	data, entry, err := h.sounds.Audio(r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	filename := entry.Name + ".wav"
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filename}))
	http.ServeContent(w, r, filename, entry.AddedAt, bytes.NewReader(data))
}

// soundsReady guards the page and every form route against an unwired service.
func (h *Handler) soundsReady(w http.ResponseWriter, r *http.Request) bool {
	if h.sounds == nil || h.settings == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

// limitForm bounds the tiny form bodies the Sounds page accepts.
func limitForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return r.ParseForm()
}

// soundsSuccess answers an htmx submission with the swapped content; a plain
// form gets the 303 redirect, so both journeys work without JavaScript.
func (h *Handler) soundsSuccess(w http.ResponseWriter, r *http.Request, flash string) {
	if isHTMX(r) {
		h.render(w, "sounds_content", h.buildSoundsView(soundsFlashMessage(flash), "", nil))
		return
	}
	http.Redirect(w, r, "/sounds?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}

// soundsFail re-renders the page with an error and, when supplied, the rejected
// Ring input. htmx only swaps 2xx responses, so a fragment error stays 200.
func (h *Handler) soundsFail(w http.ResponseWriter, r *http.Request, status int, message string, edit *editState) {
	view := h.buildSoundsView("", message, edit)
	if isHTMX(r) {
		h.render(w, "sounds_content", view)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	h.render(w, "sounds_page", view)
}

// buildSoundsView resolves the library, every fixed role and every per-kind
// Ring setting from one service snapshot.
func (h *Handler) buildSoundsView(flash, errorText string, edit *editState) soundsView {
	view := soundsView{
		Owner: h.owner, Page: "sounds", Flash: flash, Error: errorText,
		MaxUpload: fmt.Sprintf("%d MB", audio.MaxUploadBytes/(1<<20)),
	}
	if h.sounds == nil || h.settings == nil {
		return view
	}
	view.HasLibrary = true

	entries := h.sounds.List()
	for _, entry := range entries {
		origin := "Uploaded"
		if entry.Builtin {
			origin = "Built-in"
		}
		added := ""
		if !entry.AddedAt.IsZero() {
			added = entry.AddedAt.Local().Format("2 Jan 2006")
		}
		view.Sounds = append(view.Sounds, soundRowView{
			Name:     entry.Name,
			Duration: displayDuration(entry.Duration),
			Added:    added,
			Origin:   origin,
			AudioURL: "/sounds/audio/" + url.PathEscape(entry.Name),
		})
	}

	for _, role := range sound.Roles {
		key := sound.Key(role)
		value, ok := h.settings.Get(key)
		if !ok {
			continue
		}
		current := strings.TrimSpace(fmt.Sprint(value.Value))
		view.HasRoles = true
		view.Roles = append(view.Roles, soundRoleView{
			Role:        string(role),
			Label:       soundRoleLabel(role),
			Key:         key,
			Cap:         displayDuration(sound.Cap(role)),
			Display:     displayValue(current),
			Source:      string(value.Source),
			SourceLabel: sourceLabel(value.Source),
			LowerLabel:  lowerLayerLabel(value),
			Override:    value.Source == settings.SourceOverride,
			Options:     roleOptions(entries, current),
		})
	}

	byKind := make(map[string][]settingView)
	for _, value := range h.settings.List() {
		kind, ok := ringKindFor(value.Key)
		if !ok {
			continue
		}
		setting := newSettingView(value, edit)
		setting.SaveAction = "/sounds/values/" + value.Key
		setting.ResetAction = "/sounds/values/" + value.Key + "/reset"
		setting.Target = "#sounds-content"
		byKind[kind] = append(byKind[kind], setting)
	}
	for _, kind := range ringKindOrder(byKind) {
		view.HasRing = true
		view.RingKinds = append(view.RingKinds, ringKindView{
			Kind:     kind,
			Label:    ringKindLabel(kind),
			Settings: byKind[kind],
		})
	}
	return view
}

// roleOptions offers every library sound plus the silent assignment. A stored
// value that is not in the library (a config path, for example) is shown as the
// current choice so the form never hides it.
func roleOptions(entries []sound.Sound, current string) []soundOption {
	options := []soundOption{
		{Value: sound.None, Label: "None (silent)"},
		{Value: sound.BuiltinChime, Label: "Chime (built-in)"},
	}
	seen := map[string]bool{sound.None: true, sound.BuiltinChime: true}
	for _, entry := range entries {
		key := strings.ToLower(entry.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		options = append(options, soundOption{Value: entry.Name, Label: entry.Name})
	}
	matched := false
	for index := range options {
		if strings.EqualFold(options[index].Value, current) {
			options[index].Selected = true
			matched = true
		}
	}
	if current != "" && !matched {
		options = append([]soundOption{{Value: current, Label: current + " (current)", Selected: true}}, options...)
	}
	return options
}

// ringKindFor recognises a per-kind Ring setting without fixing the prefix:
// #52 may name them ring.timer.*, timer.ring.*, sounds.timer_ring.* and so on.
// Any field after the kind is accepted, so a field added later appears without
// a page change; one segment must name the kind and one must say ring, or the
// top-level section must be sounds/alert.
func ringKindFor(key string) (string, bool) {
	parts := strings.Split(key, ".")
	if len(parts) < 3 {
		return "", false
	}
	kind := ""
	hasRing := false
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "ring", "rings":
			hasRing = true
		case "timer", "timer_ring":
			kind = "timer"
		case "alarm", "alarm_ring":
			kind = "alarm"
		}
	}
	if kind == "" {
		return "", false
	}
	if !hasRing {
		switch parts[0] {
		case "ring", "rings", "sounds", "alert", "alerts":
			hasRing = true
		}
	}
	return kind, hasRing
}

// isRingSettingKey accepts a recognised Ring key, and also any ring.* key so a
// future per-kind field still opens from the page.
func isRingSettingKey(key string) bool {
	if _, ok := ringKindFor(key); ok {
		return true
	}
	return strings.HasPrefix(key, "ring.") || strings.HasPrefix(key, "rings.")
}

// ringKindOrder puts Timer then Alarm first, then any future kind, so the page
// has one stable reading order.
func ringKindOrder(byKind map[string][]settingView) []string {
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		rank := func(kind string) int {
			switch kind {
			case "timer":
				return 0
			case "alarm":
				return 1
			default:
				return 2
			}
		}
		if rank(kinds[i]) != rank(kinds[j]) {
			return rank(kinds[i]) < rank(kinds[j])
		}
		return kinds[i] < kinds[j]
	})
	return kinds
}

func ringKindLabel(kind string) string {
	switch kind {
	case "timer":
		return "Timer"
	case "alarm":
		return "Alarm"
	default:
		return strings.ToUpper(kind[:1]) + kind[1:]
	}
}

func soundRoleLabel(role sound.Role) string {
	switch role {
	case sound.RoleWake:
		return "Wake"
	case sound.RoleTimerRing:
		return "Timer Ring"
	case sound.RoleAlarmRing:
		return "Alarm Ring"
	case sound.RoleReminder:
		return "Reminder"
	case sound.RoleError:
		return "Error"
	default:
		return string(role)
	}
}

// displayDuration keeps a sub-second cue readable and a long Ring short.
func displayDuration(d time.Duration) string {
	if d <= 0 {
		return "unknown"
	}
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func displayValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(empty)"
	}
	return value
}

func soundsFlashMessage(flash string) string {
	switch flash {
	case flashUploaded:
		return "Uploaded. The sound is in the library and ready to assign."
	case flashQueued:
		return "Queued for the Pi speaker. It plays after the current reply and any pending Confirmation."
	case flashPlayed:
		return "Played on the Pi speaker."
	case flashSaved:
		return "Saved. The change applies to the next Cue or Ring."
	case flashReset:
		return "Reset. The config.yaml or code-default value applies again."
	case flashRestart:
		return "Stored. Restart the assistant for this connection setting to take effect."
	default:
		return ""
	}
}

const (
	flashUploaded = "uploaded"
	flashQueued   = "queued"
	flashPlayed   = "played"
)
