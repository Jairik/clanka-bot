package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// Flash values are a small closed set, so the redirect URL cannot reflect
// arbitrary text into the page.
const (
	flashSaved   = "saved"
	flashReset   = "reset"
	flashRestart = "restart"
)

// settingsView is the Settings page's data model. Tools and drafts are
// precomputed because the templates deliberately contain no comparison logic.
type settingsView struct {
	Owner       string
	Page        string
	Flash       string
	Error       string
	HasSettings bool
	Tools       []settingsToolView
	Agent       []settingView
	ToolWording []toolWordingView
	Quiet       []settingView
	Live        []settingView
	Restart     []settingView
	// Voice is the display name of the voice a spoken preview would use:
	// empty when the voice library is unwired. CanPreview agrees exactly with
	// the /settings/wording/preview endpoint's own check, and PreviewMaxRunes
	// is the draft bound the page should apply before uploading.
	Voice           string
	CanPreview      bool
	PreviewMaxRunes int
	// Wakeword is read-only: the allowlist and thresholds are connection
	// settings read once at startup.
	Wakeword wakewordSettingsView
}

// settingsToolView is one Tool row: the effective state plus only the
// Permission choices at or above the code declaration.
type settingsToolView struct {
	Name                  string
	Enabled               bool
	EnabledSource         string
	EnabledLabel          string
	EnabledLower          string
	EnabledOverride       bool
	Permission            tool.Permission
	PermissionLabel       string
	Declared              tool.Permission
	DeclaredLabel         string
	PermissionSource      string
	PermissionLabelSource string
	PermissionLower       string
	PermissionOverride    bool
	LocalOnly             bool
	LocalOnlyLabel        string
	Options               []permissionChoice
}

type permissionChoice struct {
	Value    string
	Label    string
	Selected bool
}

// toolWordingView groups the per-Tool wording keys that the service exposes.
// A missing key is nil, so a future Tool type only needs the definitions it
// actually has.
type toolWordingView struct {
	Name     string
	Anchor   string
	Control  *settingView
	Examples *settingView
}

// settingView is one editable setting rendered as a labelled form, never as
// implementation JSON. Edit is the current effective value unless a failed
// submission is being preserved.
type settingView struct {
	Key         string
	Anchor      string
	Label       string
	Description string
	FormKind    string
	Value       string
	Edit        string
	Display     string
	Bool        bool
	Source      string
	SourceLabel string
	LowerLabel  string
	Restart     bool
	Override    bool
	// SaveAction, ResetAction and Target let one setting template serve both
	// the Settings page and the Sounds page's Ring section.
	SaveAction  string
	ResetAction string
	Target      string
	// PreviewAction and PreviewKind are set only for the agent wording rows.
	// Spoken wording (PreviewKind "spoken") is synthesized through
	// PreviewAction; match wording (PreviewKind "match") has no audio and
	// leaves the action empty, because it is checked by typing a request. Every
	// other setting leaves both empty. The key-to-preview mapping stays
	// server-side so the app never matches setting key strings.
	PreviewAction string
	PreviewKind   string
}

// editState preserves one rejected submission for the re-rendered form.
type editState struct {
	Key   string
	Value string
	Bool  *bool
}

// settingsPage renders every Tool and every setting the service exposes.
func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildSettingsView(flashMessage(r.URL.Query().Get("flash")), "", nil))
}

// maxSettingsFormBytes bounds a settings form body. The forms carry one
// field, but the read is bounded like every other write on this surface.
const maxSettingsFormBytes = 1 << 20

// parseSettingsForm reads a bounded settings form body.
func parseSettingsForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsFormBytes)
	return r.ParseForm()
}

// settingsToolSave changes one Tool field. Both the enabled flag and the
// Permission are ordinary live settings; the Tool policy supplies the
// tighten-only check, so this handler never re-implements it.
func (h *Handler) settingsToolSave(w http.ResponseWriter, r *http.Request) {
	if !h.settingsReady(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := parseSettingsForm(w, r); err != nil {
		h.settingsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	// Local-only is code-declared. A crafted field is rejected explicitly
	// instead of being silently ignored.
	if r.Form.Has("local_only") {
		h.settingsFail(w, r, http.StatusBadRequest, "Local-only is code-declared and cannot be changed.", nil)
		return
	}
	field := r.FormValue("field")
	var update toolpolicy.Update
	switch field {
	case "enabled":
		enabled, err := strconv.ParseBool(r.FormValue("value"))
		if err != nil {
			h.settingsFail(w, r, http.StatusBadRequest, "Enabled must be true or false.", nil)
			return
		}
		update.Enabled = &enabled
	case "permission":
		permission := tool.Permission(r.FormValue("value"))
		update.Permission = &permission
	default:
		h.settingsFail(w, r, http.StatusBadRequest, "Choose the enabled or permission field.", nil)
		return
	}
	if _, err := h.tools.Set(r.Context(), name, update); err != nil {
		h.settingsFail(w, r, toolErrorStatus(err), err.Error(), nil)
		return
	}
	h.settingsSuccess(w, r, flashSaved)
}

// settingsToolReset removes one field's override. It resets only that field,
// so resetting enabled does not also discard a Permission tightening.
func (h *Handler) settingsToolReset(w http.ResponseWriter, r *http.Request) {
	if !h.settingsReady(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := parseSettingsForm(w, r); err != nil {
		h.settingsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	var key string
	switch r.FormValue("field") {
	case "enabled":
		key = toolpolicy.EnabledKey(name)
	case "permission":
		key = toolpolicy.PermissionKey(name)
	default:
		h.settingsFail(w, r, http.StatusBadRequest, "Choose the enabled or permission field.", nil)
		return
	}
	if _, err := h.settings.Reset(r.Context(), key); err != nil {
		h.settingsFail(w, r, settingsErrorStatus(err), err.Error(), nil)
		return
	}
	h.settingsSuccess(w, r, flashReset)
}

// settingsValueSave writes one generic setting through the same typed
// SetJSON path as the API. The editor is chosen from the effective value's Go
// type, so a new sound or other setting appears without a page change.
func (h *Handler) settingsValueSave(w http.ResponseWriter, r *http.Request) {
	if !h.settingsReady(w, r) {
		return
	}
	key := r.PathValue("key")
	value, ok := h.settings.Get(key)
	if !ok {
		h.settingsFail(w, r, http.StatusNotFound, "Unknown setting.", nil)
		return
	}
	if err := parseSettingsForm(w, r); err != nil {
		h.settingsFail(w, r, http.StatusBadRequest, "Could not read the form.", nil)
		return
	}
	raw, edit, err := settingFormValue(value, r.FormValue("value"))
	if err != nil {
		h.settingsFail(w, r, http.StatusBadRequest, err.Error(), edit)
		return
	}
	if _, err := h.settings.SetJSON(r.Context(), key, raw); err != nil {
		h.settingsFail(w, r, http.StatusBadRequest, err.Error(), edit)
		return
	}
	flash := flashSaved
	if value.Kind == settings.RestartRequired {
		flash = flashRestart
	}
	h.settingsSuccess(w, r, flash)
}

// settingsValueReset deletes one override; the config.yaml or code default
// layer applies again.
func (h *Handler) settingsValueReset(w http.ResponseWriter, r *http.Request) {
	if !h.settingsReady(w, r) {
		return
	}
	key := r.PathValue("key")
	value, ok := h.settings.Get(key)
	if !ok {
		h.settingsFail(w, r, http.StatusNotFound, "Unknown setting.", nil)
		return
	}
	if _, err := h.settings.Reset(r.Context(), key); err != nil {
		h.settingsFail(w, r, settingsErrorStatus(err), err.Error(), nil)
		return
	}
	flash := flashReset
	if value.Kind == settings.RestartRequired {
		flash = flashRestart
	}
	h.settingsSuccess(w, r, flash)
}

// settingsReady guards every form route against an unwired service.
func (h *Handler) settingsReady(w http.ResponseWriter, r *http.Request) bool {
	if h.settings == nil || h.tools == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

// settingsSuccess answers with the refreshed view, so the app can replace the
// page state from one response.
func (h *Handler) settingsSuccess(w http.ResponseWriter, r *http.Request, flash string) {
	h.writeJSON(w, http.StatusOK, h.buildSettingsView(flashMessage(flash), "", nil))
}

// settingsFail re-renders the page with an error and, when supplied, the
// rejected input. The status is the real one, so the app can tell a refused
// write from a saved one.
func (h *Handler) settingsFail(w http.ResponseWriter, r *http.Request, status int, message string, edit *editState) {
	view := h.buildSettingsView("", message, edit)
	h.writeJSON(w, status, view)
}

func flashMessage(flash string) string {
	switch flash {
	case flashSaved:
		return "Saved. The change applies on the next request."
	case flashReset:
		return "Reset. The config.yaml or code-default value applies again."
	case flashRestart:
		return "Stored. Restart the assistant for this connection setting to take effect."
	default:
		return ""
	}
}

// buildSettingsView resolves every Tool, the known wording groups and every
// remaining setting from one service snapshot.
func (h *Handler) buildSettingsView(flash, errorText string, edit *editState) settingsView {
	view := settingsView{Owner: h.owner, Page: "settings", Flash: flash, Error: errorText, Wakeword: h.wakewordSettings()}
	// The voice fields are independent of the settings service: an unwired
	// library is reported as no voice and no preview rather than a panic, and
	// CanPreview applies the same nil checks the preview endpoint does. The
	// label comes from speechVoiceName, so it names the voice the endpoint
	// actually synthesizes with (a stale active voice reports the default).
	view.Voice = h.speechVoiceName()
	if h.voices != nil {
		view.CanPreview = h.voicePreview != nil
		if view.CanPreview {
			view.PreviewMaxRunes = voicesample.MaxPreviewRunes
		}
	}
	if h.settings == nil {
		return view
	}
	view.HasSettings = true

	seen := make(map[string]bool)
	if h.tools != nil {
		for _, state := range h.tools.List() {
			view.Tools = append(view.Tools, h.toolView(state))
			seen[toolpolicy.EnabledKey(state.Name)] = true
			seen[toolpolicy.PermissionKey(state.Name)] = true
		}
	}

	values := h.settings.List()
	for _, value := range values {
		switch value.Key {
		case wording.FillerKey, wording.UnavailableKey:
			seen[value.Key] = true
			setting := newSettingView(value, edit)
			setting.PreviewAction = wordingPreviewAction
			setting.PreviewKind = wordingPreviewSpoken
			view.Agent = append(view.Agent, setting)
		case wording.PrefixesKey:
			seen[value.Key] = true
			setting := newSettingView(value, edit)
			setting.PreviewKind = wordingPreviewMatch
			view.Agent = append(view.Agent, setting)
		}
	}

	// Per-Tool wording is grouped by Tool name, but discovered from the
	// setting keys themselves so a Tool that appears later needs no page edit.
	wordingByTool := make(map[string]*toolWordingView)
	for _, value := range values {
		if name, ok := wordingToolKey(value.Key, ".control_phrases"); ok {
			seen[value.Key] = true
			entry := ensureToolWording(wordingByTool, name)
			setting := newSettingView(value, edit)
			entry.Control = &setting
		}
		if name, ok := wordingToolKey(value.Key, ".examples"); ok {
			seen[value.Key] = true
			entry := ensureToolWording(wordingByTool, name)
			setting := newSettingView(value, edit)
			entry.Examples = &setting
		}
	}
	names := make([]string, 0, len(wordingByTool))
	for name := range wordingByTool {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		view.ToolWording = append(view.ToolWording, *wordingByTool[name])
	}

	for _, value := range values {
		// The equalizer section owns these atomic profiles.
		if seen[value.Key] || isToolPolicyKey(value.Key) || strings.HasPrefix(value.Key, "audio.equalizer") {
			continue
		}
		setting := newSettingView(value, edit)
		if strings.HasPrefix(value.Key, "quiet.") {
			view.Quiet = append(view.Quiet, setting)
			continue
		}
		if value.Kind == settings.RestartRequired {
			view.Restart = append(view.Restart, setting)
		} else {
			view.Live = append(view.Live, setting)
		}
	}
	return view
}

func (h *Handler) toolView(state toolpolicy.State) settingsToolView {
	enabledKey, permissionKey := toolpolicy.EnabledKey(state.Name), toolpolicy.PermissionKey(state.Name)
	enabledValue, _ := h.settings.Get(enabledKey)
	permissionValue, _ := h.settings.Get(permissionKey)
	view := settingsToolView{
		Name:                  state.Name,
		Enabled:               state.Enabled,
		EnabledSource:         string(state.EnabledSource),
		EnabledLabel:          sourceLabel(state.EnabledSource),
		EnabledLower:          lowerLayerLabel(enabledValue),
		EnabledOverride:       state.EnabledSource == settings.SourceOverride,
		Permission:            state.Permission,
		PermissionLabel:       permissionLabel(state.Permission),
		Declared:              state.Declared,
		DeclaredLabel:         permissionLabel(state.Declared),
		PermissionSource:      string(state.PermissionSource),
		PermissionLabelSource: sourceLabel(state.PermissionSource),
		PermissionLower:       lowerLayerLabel(permissionValue),
		PermissionOverride:    state.PermissionSource == settings.SourceOverride,
		LocalOnly:             state.LocalOnly,
		LocalOnlyLabel:        yesNo(state.LocalOnly),
	}
	// Offer only the levels at or above the code declaration: the lower levels
	// are a loosening, and the server rejects them even if crafted.
	for _, permission := range []tool.Permission{tool.PermissionRead, tool.PermissionAct, tool.PermissionConfirm} {
		if !permission.AtLeast(state.Declared) {
			continue
		}
		view.Options = append(view.Options, permissionChoice{
			Value:    string(permission),
			Label:    permissionLabel(permission),
			Selected: permission == state.Permission,
		})
	}
	return view
}

func ensureToolWording(byTool map[string]*toolWordingView, name string) *toolWordingView {
	entry, ok := byTool[name]
	if !ok {
		entry = &toolWordingView{Name: name, Anchor: "tool-wording-" + name}
		byTool[name] = entry
	}
	return entry
}

// wordingToolKey parses tools.<name>.<suffix>, where the Tool name may itself
// contain dots (time.now).
func wordingToolKey(key, suffix string) (string, bool) {
	const prefix = "tools."
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	if name == "" {
		return "", false
	}
	return name, true
}

func isToolPolicyKey(key string) bool {
	if !strings.HasPrefix(key, "tools.") {
		return false
	}
	return strings.HasSuffix(key, ".enabled") || strings.HasSuffix(key, ".permission")
}

func newSettingView(value settings.Value, edit *editState) settingView {
	view := settingView{
		Key:         value.Key,
		Anchor:      "setting-" + value.Key,
		Label:       settingLabel(value.Key),
		Description: value.Description,
		Source:      string(value.Source),
		SourceLabel: sourceLabel(value.Source),
		LowerLabel:  lowerLayerLabel(value),
		Restart:     value.Kind == settings.RestartRequired,
		Override:    value.Source == settings.SourceOverride,
		SaveAction:  "/settings/values/" + value.Key,
		ResetAction: "/settings/values/" + value.Key + "/reset",
		Target:      "#settings-content",
	}
	switch typed := value.Value.(type) {
	case []string:
		view.FormKind = "lines"
		view.Value = strings.Join(typed, ", ")
		view.Edit = strings.Join(typed, "\n")
		if len(typed) == 0 {
			view.Value = "(empty)"
		}
	case bool:
		view.FormKind = "bool"
		view.Bool = typed
		view.Edit = strconv.FormatBool(typed)
		view.Value = onOff(typed)
	case int:
		view.FormKind = "number"
		view.Value = strconv.Itoa(typed)
		view.Edit = view.Value
	case float64:
		view.FormKind = "decimal"
		view.Value = strconv.FormatFloat(typed, 'g', -1, 64)
		view.Edit = view.Value
	case time.Duration:
		view.FormKind = "duration"
		view.Value = typed.String()
		view.Edit = view.Value
	case string:
		view.FormKind = "text"
		view.Value = typed
		view.Edit = typed
		if typed == "" {
			view.Value = "(empty)"
		}
	default:
		view.FormKind = "text"
		view.Value = fmt.Sprint(typed)
		view.Edit = view.Value
	}
	view.Display = view.Value
	if edit != nil && edit.Key == value.Key {
		view.Edit = edit.Value
		if edit.Bool != nil {
			view.Bool = *edit.Bool
		}
	}
	return view
}

// settingFormValue converts one HTML field to the JSON representation the
// settings service accepts. Textareas are one entry per line; blank lines are
// ignored because they are formatting, not values.
func settingFormValue(value settings.Value, text string) (json.RawMessage, *editState, error) {
	edit := &editState{Key: value.Key, Value: text}
	switch value.Value.(type) {
	case []string:
		lines := make([]string, 0, strings.Count(text, "\n")+1)
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
		raw, err := json.Marshal(lines)
		return raw, edit, err
	case bool:
		parsed, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return nil, edit, fmt.Errorf("must be true or false")
		}
		edit.Bool = &parsed
		raw, err := json.Marshal(parsed)
		return raw, edit, err
	case int:
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return nil, edit, fmt.Errorf("must be a whole number")
		}
		raw, err := json.Marshal(parsed)
		return raw, edit, err
	case float64:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, edit, fmt.Errorf("must be a number")
		}
		raw, err := json.Marshal(parsed)
		return raw, edit, err
	case time.Duration:
		if _, err := time.ParseDuration(strings.TrimSpace(text)); err != nil {
			return nil, edit, fmt.Errorf("must be a duration such as 9m0s")
		}
		raw, err := json.Marshal(strings.TrimSpace(text))
		return raw, edit, err
	case string:
		raw, err := json.Marshal(text)
		return raw, edit, err
	default:
		return nil, edit, fmt.Errorf("this setting has no form editor")
	}
}

func settingsErrorStatus(err error) int {
	if errors.Is(err, settings.ErrUnknownSetting) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func toolErrorStatus(err error) int {
	if errors.Is(err, toolpolicy.ErrUnknownTool) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func sourceLabel(source settings.Source) string {
	switch source {
	case settings.SourceOverride:
		return "Override"
	case settings.SourceConfig:
		return "config.yaml"
	default:
		return "Default"
	}
}

// lowerLayerLabel names what a Reset would reveal, so the button is explicit
// about the lower layer rather than a generic "clear".
func lowerLayerLabel(value settings.Value) string {
	if value.Config != nil {
		return "config.yaml"
	}
	return "code default"
}

func permissionLabel(permission tool.Permission) string {
	if permission == "" {
		return "Unknown"
	}
	return strings.ToUpper(string(permission)[:1]) + string(permission)[1:]
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// Labels are for display only; the exact key is shown beside every setting so
// a future key is understandable before this list learns a nicer name.
var settingLabels = map[string]string{
	"agent.model":               "Model",
	"agent.filler":              "Filler",
	"agent.unavailable_message": "Unavailable message",
	"agent.request_prefixes":    "Request prefixes",
	"timezone":                  "Timezone",
	"name_match.threshold":      "Name match threshold",
	"http.addr":                 "HTTP address",
	"debug.addr":                "Debug address",
	"dashboard.addr":            "Dashboard address",
	"tts.addr":                  "TTS address",
	"wakeword.addr":             "Wake word address",
	"stt.addr":                  "STT address",
	"embed.url":                 "Embedding URL",
	"capture.source":            "Capture source",
	"playback.sink":             "Playback sink",
}

func settingLabel(key string) string {
	if label, ok := settingLabels[key]; ok {
		return label
	}
	segment := key
	if index := strings.LastIndex(key, "."); index >= 0 {
		segment = key[index+1:]
	}
	words := strings.Fields(strings.ReplaceAll(segment, "_", " "))
	for index, word := range words {
		word = strings.ToLower(word)
		if index == 0 {
			word = strings.ToUpper(word[:1]) + word[1:]
		}
		words[index] = word
	}
	if len(words) == 0 {
		return key
	}
	return strings.Join(words, " ")
}
