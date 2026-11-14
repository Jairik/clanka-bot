package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/phrasebook"
)

// Phrase book flash values. They are internal names, never user text.
const (
	flashPhraseSaved     = "phrase-saved"
	flashPhraseReset     = "phrase-reset"
	flashPhraseGenerated = "phrase-generated"
	flashPhraseCleared   = "phrase-cleared"
)

// phrasebookView is the Phrase book page data model. One selected Persona (or
// the no-Persona default layer) is shown at a time, with every overridable Tool
// and the phrase that will actually be spoken.
type phrasebookView struct {
	Owner         string
	Page          string
	Flash         string
	Error         string
	HasService    bool
	HasGenerator  bool
	ActivePersona string
	Selected      string
	Personas      []phrasebookPersonaView
	Slots         []phrasebookSlotView
	Total         int
	Overridden    int
	Generated     int
	Manual        int
}

// phrasebookPersonaView is one selectable Persona.
type phrasebookPersonaView struct {
	Name   string
	Active bool
	Count  int
}

// phrasebookSlotView is one Tool and the phrase that will be spoken for it.
type phrasebookSlotView struct {
	Tool        string
	Group       string
	Description string
	Default     string
	Phrase      string
	Source      string
	Overridden  bool
}

// phrasebookPage renders the selected Persona's phrase book.
func (h *Handler) phrasebookPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildPhrasebookView(r.URL.Query().Get("persona"), "", ""))
}

// phrasebookSave stores one phrase. The selected Persona comes from the path;
// the Tool and text come from the form.
func (h *Handler) phrasebookSave(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.phrasebookFail(w, r, http.StatusBadRequest, "Could not read the form.", "")
		return
	}
	name := r.PathValue("persona")
	toolName := strings.TrimSpace(r.FormValue("tool"))
	if !phrasebook.Overridable(toolName) {
		h.phrasebookFail(w, r, http.StatusBadRequest, "That Tool does not have an overridable reply.", name)
		return
	}
	if _, err := h.phrases.Set(r.Context(), name, toolName, r.FormValue("phrase"), phrasebook.SourceManual); err != nil {
		status, message := phrasebookErrorStatus(err)
		h.logger.Warn("phrase book save failed", "persona", name, "tool", toolName, "error", err.Error())
		h.phrasebookFail(w, r, status, message, name)
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildPhrasebookView(name, flashPhraseSaved, ""))
}

// phrasebookReset removes one override, restoring the shipped default.
func (h *Handler) phrasebookReset(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.phrasebookFail(w, r, http.StatusBadRequest, "Could not read the form.", "")
		return
	}
	name := r.PathValue("persona")
	toolName := strings.TrimSpace(r.FormValue("tool"))
	if err := h.phrases.Delete(r.Context(), name, toolName); err != nil {
		h.logger.Warn("phrase book reset failed", "persona", name, "tool", toolName, "error", err.Error())
		h.phrasebookFail(w, r, http.StatusBadRequest, "The phrase could not be reset.", name)
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildPhrasebookView(name, flashPhraseReset, ""))
}

// phrasebookClear removes every override for one Persona.
func (h *Handler) phrasebookClear(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	name := r.PathValue("persona")
	if _, ok := h.personaByName(name); !ok {
		h.phrasebookFail(w, r, http.StatusNotFound, "That persona does not exist.", name)
		return
	}
	for _, entry := range h.phrases.Entries(name) {
		if err := h.phrases.Delete(r.Context(), name, entry.Tool); err != nil {
			h.logger.Warn("phrase book clear failed", "persona", name, "tool", entry.Tool, "error", err.Error())
			h.phrasebookFail(w, r, http.StatusInternalServerError, "The phrase book could not be cleared.", name)
			return
		}
	}
	h.writeJSON(w, http.StatusOK, h.buildPhrasebookView(name, flashPhraseCleared, ""))
}

// phrasebookGenerate asks the Agent provider for a whole in-character set and
// replaces the Persona's overrides with it.
func (h *Handler) phrasebookGenerate(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	name := r.PathValue("persona")
	p, ok := h.personaByName(name)
	if !ok {
		h.phrasebookFail(w, r, http.StatusNotFound, "That persona does not exist.", name)
		return
	}
	if h.phraseGen == nil {
		h.phrasebookFail(w, r, http.StatusServiceUnavailable, "The Agent provider is not configured, so phrases cannot be generated.", name)
		return
	}
	if len(h.phraseSlots) == 0 {
		h.phrasebookFail(w, r, http.StatusServiceUnavailable, "No overridable Tools are registered.", name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	generated, err := phrasebook.Generate(ctx, p.Name, p.Personality, h.phraseSlots, h.phraseGen)
	if err != nil {
		status := http.StatusBadGateway
		message := "The agent couldn't generate phrases. Check the model and key on Secrets."
		if errors.Is(err, phrasebook.ErrNoCompleter) {
			status = http.StatusServiceUnavailable
			message = "The Agent provider is not configured, so phrases cannot be generated."
		}
		h.logger.Warn("phrase book generation failed", "persona", name, "error", err.Error())
		h.phrasebookFail(w, r, status, message, name)
		return
	}
	if _, err := h.phrases.Replace(r.Context(), p.Name, generated, phrasebook.SourceLLM); err != nil {
		status, message := phrasebookErrorStatus(err)
		h.logger.Warn("phrase book generation save failed", "persona", name, "error", err.Error())
		h.phrasebookFail(w, r, status, message, name)
		return
	}
	h.logger.Info("phrase book generated from dashboard", "persona", p.Name, "phrases", len(generated))
	h.writeJSON(w, http.StatusOK, h.buildPhrasebookView(p.Name, flashPhraseGenerated, ""))
}

func (h *Handler) phrasebookReady(w http.ResponseWriter, r *http.Request) bool {
	if h.phrases == nil || h.personas == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (h *Handler) phrasebookFail(w http.ResponseWriter, r *http.Request, status int, message, persona string) {
	if persona == "" {
		persona = r.URL.Query().Get("persona")
	}
	h.writeJSON(w, status, h.buildPhrasebookView(persona, "", message))
}

func phrasebookErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, phrasebook.ErrUnknownTool):
		return http.StatusBadRequest, "That Tool does not have an overridable reply."
	case errors.Is(err, phrasebook.ErrInvalidPhrase):
		return http.StatusBadRequest, "A phrase must be one non-empty line up to 80 characters."
	default:
		return http.StatusInternalServerError, "The phrase book could not be saved."
	}
}

// buildPhrasebookView precomputes every row. selected is the Persona the page
// is showing; empty means the shipped defaults with no Persona selected.
func (h *Handler) buildPhrasebookView(selected, flash, problem string) phrasebookView {
	view := phrasebookView{
		Owner:        h.owner,
		Page:         "phrasebook",
		Flash:        phrasebookFlashMessage(flash),
		Error:        problem,
		HasService:   h.phrases != nil && h.personas != nil,
		HasGenerator: h.phraseGen != nil,
		Selected:     strings.TrimSpace(selected),
		Slots:        []phrasebookSlotView{},
	}
	if h.personas != nil {
		if current, ok := h.personas.Current(); ok {
			view.ActivePersona = current.Persona.Name
		}
		for _, p := range h.personas.List() {
			count := 0
			if h.phrases != nil {
				count = len(h.phrases.Entries(p.Name))
			}
			view.Personas = append(view.Personas, phrasebookPersonaView{
				Name:   p.Name,
				Active: strings.EqualFold(p.Name, view.ActivePersona),
				Count:  count,
			})
		}
	}
	if view.Selected == "" {
		view.Selected = view.ActivePersona
	}
	entries := make(map[string]phrasebook.Entry)
	if view.Selected != "" && h.phrases != nil {
		for _, entry := range h.phrases.Entries(view.Selected) {
			entries[entry.Tool] = entry
		}
	}
	slots := append([]phrasebook.Slot(nil), h.phraseSlots...)
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].Group != slots[j].Group {
			return slots[i].Group < slots[j].Group
		}
		return slots[i].Tool < slots[j].Tool
	})
	for _, slot := range slots {
		row := phrasebookSlotView{
			Tool:        slot.Tool,
			Group:       slot.Group,
			Description: slot.Description,
			Default:     slot.Default,
			Phrase:      slot.Default,
			Source:      string(phrasebook.SourceDefault),
		}
		if entry, ok := entries[slot.Tool]; ok {
			row.Phrase = entry.Phrase
			row.Source = string(entry.Source)
			row.Overridden = true
			view.Overridden++
			switch entry.Source {
			case phrasebook.SourceLLM:
				view.Generated++
			default:
				view.Manual++
			}
		}
		view.Slots = append(view.Slots, row)
	}
	view.Total = len(view.Slots)
	return view
}

// personaByName resolves one Persona case-insensitively.
func (h *Handler) personaByName(name string) (persona.Persona, bool) {
	if h.personas == nil {
		return persona.Persona{}, false
	}
	return h.personas.Get(strings.TrimSpace(name))
}

// phrasebookFlashMessage maps one flash name to the page's label.
func phrasebookFlashMessage(name string) string {
	switch name {
	case flashPhraseSaved:
		return "Phrase saved."
	case flashPhraseReset:
		return "Phrase reset to the default."
	case flashPhraseGenerated:
		return "Phrase book generated. Review the rows before the next request."
	case flashPhraseCleared:
		return "All overrides cleared; the defaults are back."
	default:
		return ""
	}
}
