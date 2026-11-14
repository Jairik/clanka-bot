package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// personasView is the Persona page's data model. The active summary and every
// form action are precomputed so the template stays presentational.
type personasView struct {
	Owner      string
	Page       string
	Flash      string
	Error      string
	HasService bool
	Active     *personaActiveView
	New        personaFormView
	Rows       []personaRowView
}

// personaActiveView is the selected Persona plus the settings that really
// apply. Notes name every difference, so the page never implies an exact
// bundle after a manual override or an edit.
type personaActiveView struct {
	Name           string
	Voice          string
	EffectiveVoice string
	Exact          bool
	Notes          []string
}

// personaRowView is one stored Persona with its edit form and select action.
type personaRowView struct {
	Name         string
	Voice        string
	VoiceStatus  string
	Active       bool
	SelectAction string
	Edit         personaFormView
}

// personaFormView is one create or edit form. NameLocked is true on an edit:
// the name is the stable row key and is never renamed through the form.
type personaFormView struct {
	Action      string
	Heading     string
	FormID      string
	Name        string
	NameLocked  bool
	Voice       string
	Personality string
	Filler      string
	Unavailable string
	Submit      string
	Voices      []personaVoiceOption
}

// personaVoiceOption is one ready voice in a Persona form, plus the currently
// stored voice even when it has gone stale so the edit form can say why.
type personaVoiceOption struct {
	Name     string
	Label    string
	Selected bool
}

// personasPage renders the library, the active summary and every form.
func (h *Handler) personasPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildPersonasView(personasFlashMessage(r.URL.Query().Get("flash")), "", nil, "", nil))
}

// personasCreate validates and stores one Persona. Selecting is a separate
// action: creating does not change the speaking voice or wording.
func (h *Handler) personasCreate(w http.ResponseWriter, r *http.Request) {
	if !h.personasReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.personasFail(w, r, http.StatusBadRequest, "Could not read the form.", nil, "", nil)
		return
	}
	in := personaInput(r)
	p, err := h.personas.Create(r.Context(), in)
	if err != nil {
		status, message := personaErrorStatus(err)
		h.logPersonaError("create", err)
		form := h.personaForm("/personas", "Create a persona", "", false, in)
		// Keep the typed name so a refused create can be corrected in place;
		// the form keeps its "new" id.
		form.Name = in.Name
		h.personasFail(w, r, status, message, &form, "", nil)
		return
	}
	h.logger.Info("persona created from dashboard", "name", p.Name)
	h.personasSuccess(w, r, flashPersonaCreated)
}

// personasUpdate saves the voice, personality and wording of an existing
// Persona. The name is taken from the path and cannot be changed by the form.
func (h *Handler) personasUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.personasReady(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := limitForm(w, r); err != nil {
		h.personasFail(w, r, http.StatusBadRequest, "Could not read the form.", nil, name, nil)
		return
	}
	in := personaInput(r)
	in.Name = name
	p, err := h.personas.Update(r.Context(), name, in)
	if err != nil {
		status, message := personaErrorStatus(err)
		h.logPersonaError("update", err)
		form := h.personaForm("/personas/"+url.PathEscape(name), "Edit persona", name, true, in)
		h.personasFail(w, r, status, message, nil, name, &form)
		return
	}
	h.logger.Info("persona updated from dashboard", "name", p.Name)
	h.personasSuccess(w, r, flashPersonaSaved)
}

// personasSelect applies the whole bundle in one settings transaction. A stale
// voice rejects before any setting changes, and the page says so.
func (h *Handler) personasSelect(w http.ResponseWriter, r *http.Request) {
	if !h.personasReady(w, r) {
		return
	}
	name := r.PathValue("name")
	p, err := h.personas.Select(r.Context(), name)
	if err != nil {
		status, message := personaErrorStatus(err)
		h.logPersonaError("select", err)
		h.personasFail(w, r, status, message, nil, "", nil)
		return
	}
	h.logger.Info("persona selected from dashboard", "name", p.Name, "voice", p.Voice)
	h.personasSuccess(w, r, flashPersonaSelected)
}

// personasClear removes only the active marker. The applied voice and wording
// stay; the page explains that rather than implying a rollback.
func (h *Handler) personasClear(w http.ResponseWriter, r *http.Request) {
	if !h.personasReady(w, r) {
		return
	}
	if err := h.personas.ClearActive(r.Context()); err != nil {
		status, message := personaErrorStatus(err)
		h.logPersonaError("clear", err)
		h.personasFail(w, r, status, message, nil, "", nil)
		return
	}
	h.personasSuccess(w, r, flashPersonaCleared)
}

// personasReady guards every write against an unwired service. The page itself
// renders read-only when the library is absent.
func (h *Handler) personasReady(w http.ResponseWriter, r *http.Request) bool {
	if h.personas == nil || h.settings == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

// personasSuccess answers with the refreshed view, so the app can replace the
// page state from one response.
func (h *Handler) personasSuccess(w http.ResponseWriter, r *http.Request, flash string) {
	h.writeJSON(w, http.StatusOK, h.buildPersonasView(personasFlashMessage(flash), "", nil, "", nil))
}

// personasFail re-renders the page with an error and, when supplied, the
// rejected create or edit form values. The status is the real one, so the app
// can tell a refused write from a saved one.
func (h *Handler) personasFail(w http.ResponseWriter, r *http.Request, status int, message string, create *personaFormView, editName string, edit *personaFormView) {
	view := h.buildPersonasView("", message, create, editName, edit)
	h.writeJSON(w, status, view)
}

// buildPersonasView resolves the library, the active summary and every form
// from one snapshot. Rejected form values override the stored record.
func (h *Handler) buildPersonasView(flash, errorText string, create *personaFormView, editName string, edit *personaFormView) personasView {
	view := personasView{Owner: h.owner, Page: "personas", Flash: flash, Error: errorText}
	if h.personas == nil {
		return view
	}
	view.HasService = true
	if create != nil {
		view.New = *create
	} else {
		view.New = h.personaForm("/personas", "Create a persona", "", false, persona.Input{
			Voice:       h.personaDefaultVoice(),
			Filler:      h.effectiveWording(wording.FillerKey),
			Unavailable: h.effectiveWording(wording.UnavailableKey),
		})
	}

	if current, ok := h.personas.Current(); ok {
		active := &personaActiveView{
			Name:           current.Persona.Name,
			Voice:          current.Persona.Voice,
			EffectiveVoice: current.EffectiveVoice,
			Exact:          current.Exact,
		}
		if current.VoiceDiffers {
			if current.EffectiveVoice != "" {
				active.Notes = append(active.Notes, fmt.Sprintf("The voice is set to %s instead of %s.", current.EffectiveVoice, current.Persona.Voice))
			} else {
				active.Notes = append(active.Notes, "The persona's voice is not available, so the default voice speaks.")
			}
		}
		if current.FillerDiffers {
			active.Notes = append(active.Notes, "The Filler wording has been changed since this persona was selected.")
		}
		if current.UnavailableDiffers {
			active.Notes = append(active.Notes, "The Unavailable message has been changed since this persona was selected.")
		}
		if !current.Exact {
			active.Notes = append(active.Notes, "Select the persona again to apply its stored voice and wording.")
		}
		view.Active = active
	}

	for _, p := range h.personas.List() {
		row := personaRowView{
			Name:         p.Name,
			Voice:        p.Voice,
			Active:       view.Active != nil && strings.EqualFold(view.Active.Name, p.Name),
			SelectAction: "/personas/" + url.PathEscape(p.Name) + "/select",
		}
		if h.voices == nil {
			row.VoiceStatus = "missing"
		} else if voice, ok := h.voices.Get(p.Voice); ok {
			row.VoiceStatus = voice.Status()
		} else {
			row.VoiceStatus = "missing"
		}
		in := persona.Input{Name: p.Name, Voice: p.Voice, Personality: p.Personality, Filler: p.Filler, Unavailable: p.Unavailable}
		row.Edit = h.personaForm("/personas/"+url.PathEscape(p.Name), "Edit persona", p.Name, true, in)
		if edit != nil && strings.EqualFold(editName, p.Name) {
			row.Edit = *edit
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// personaForm builds one form's values and ready-voice choices.
func (h *Handler) personaForm(action, heading, name string, locked bool, in persona.Input) personaFormView {
	form := personaFormView{
		Action:      action,
		Heading:     heading,
		FormID:      personaFormID(name),
		Name:        name,
		NameLocked:  locked,
		Voice:       in.Voice,
		Personality: in.Personality,
		Filler:      in.Filler,
		Unavailable: in.Unavailable,
		Voices:      h.personaVoiceOptions(in.Voice),
	}
	if locked {
		form.Submit = "Save persona"
	} else {
		form.Submit = "Create persona"
	}
	return form
}

// personaVoiceOptions lists ready states and the stored voice even when it has
// gone stale, so an edit can explain why saving will be refused until a ready
// voice is chosen.
func (h *Handler) personaVoiceOptions(selected string) []personaVoiceOption {
	var options []personaVoiceOption
	if h.voices == nil {
		return options
	}
	seen := make(map[string]bool)
	for _, voice := range h.voices.List() {
		if !voice.Ready() {
			continue
		}
		seen[strings.ToLower(voice.Name)] = true
		options = append(options, personaVoiceOption{
			Name:     voice.Name,
			Label:    voice.Name,
			Selected: strings.EqualFold(voice.Name, selected),
		})
	}
	if selected != "" && !seen[strings.ToLower(selected)] {
		options = append(options, personaVoiceOption{
			Name:     selected,
			Label:    selected + " (not ready; choose a ready voice)",
			Selected: true,
		})
	}
	return options
}

// personaFormID builds a stable HTML id from the name; a new form uses
// "persona-new" because its name is not chosen yet.
func personaFormID(name string) string {
	if strings.TrimSpace(name) == "" {
		return "persona-new"
	}
	var id strings.Builder
	id.WriteString("persona-")
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			id.WriteRune(r)
		default:
			id.WriteByte('-')
		}
	}
	return id.String()
}

// personaDefaultVoice picks a ready default for a new Persona.
func (h *Handler) personaDefaultVoice() string {
	if h.voices == nil {
		return ""
	}
	if voice, ok := h.voices.Get(h.voices.DefaultName()); ok && voice.Ready() {
		return voice.Name
	}
	for _, voice := range h.voices.List() {
		if voice.Ready() {
			return voice.Name
		}
	}
	return ""
}

// effectiveWording returns the live wording for a new form's starting value,
// so creating a Persona starts from what the assistant already says.
func (h *Handler) effectiveWording(key string) string {
	if h.settings == nil {
		return ""
	}
	return h.settings.String(key)
}

func (h *Handler) logPersonaError(action string, err error) {
	var target *persona.Error
	if errors.As(err, &target) {
		h.logger.Warn("persona "+action+" rejected", "error", err.Error())
		return
	}
	h.logger.Error("persona "+action+" failed", "error", err.Error())
}

// personaInput reads the five form fields. Voice and wording are plain text
// because the template renders a select and textareas.
func personaInput(r *http.Request) persona.Input {
	return persona.Input{
		Name:        r.FormValue("name"),
		Voice:       r.FormValue("voice"),
		Personality: r.FormValue("personality"),
		Filler:      r.FormValue("filler"),
		Unavailable: r.FormValue("unavailable"),
	}
}

// personaErrorStatus maps a library rejection to an HTTP status and safe text.
func personaErrorStatus(err error) (int, string) {
	message := persona.Message(err)
	switch {
	case errors.Is(err, persona.ErrUnknownPersona):
		if message == "" {
			message = "Unknown persona."
		}
		return http.StatusNotFound, message
	case errors.Is(err, persona.ErrPersonaExists):
		if message == "" {
			message = "A persona with that name already exists."
		}
		return http.StatusConflict, message
	case errors.Is(err, persona.ErrInvalidPersona), errors.Is(err, persona.ErrVoiceUnavailable):
		if message == "" {
			message = "The persona could not be saved. Check the fields and the selected voice."
		}
		return http.StatusBadRequest, message
	case errors.Is(err, persona.ErrNoSettings):
		return http.StatusServiceUnavailable, "The settings service is not available."
	default:
		return http.StatusInternalServerError, "The request failed. Check the assistant log for the reason."
	}
}

func personasFlashMessage(flash string) string {
	switch flash {
	case flashPersonaCreated:
		return "Persona created. Select it to apply its voice, personality, Filler and Unavailable message."
	case flashPersonaSaved:
		return "Persona saved. The personality prompt applies on the next turn; select the persona again to apply a changed voice or wording."
	case flashPersonaSelected:
		return "Selected. The voice, personality, Filler and Unavailable message switch together from the next turn."
	case flashPersonaCleared:
		return "Active persona cleared. The current voice and wording stay as they are."
	default:
		return ""
	}
}

const (
	flashPersonaCreated  = "persona-created"
	flashPersonaSaved    = "persona-saved"
	flashPersonaSelected = "persona-selected"
	flashPersonaCleared  = "persona-cleared"
)
