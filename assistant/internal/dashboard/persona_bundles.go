package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/phrasebook"
)

const personaBundleVersion = 1

// A portable bundle includes the voice name and all overrides, including hidden Tools.
type personaBundle struct {
	Version int                         `json:"version"`
	Persona persona.Input               `json:"persona"`
	Phrases []phrasebook.PortablePhrase `json:"phrases"`
}

func (h *Handler) personaBundle(name string) (personaBundle, bool) {
	p, ok := h.personaByName(name)
	if !ok {
		return personaBundle{}, false
	}
	entries := h.phrases.Entries(p.Name)
	phrases := make([]phrasebook.PortablePhrase, 0, len(entries))
	for _, entry := range entries {
		phrases = append(phrases, phrasebook.PortablePhrase{Tool: entry.Tool, Phrase: entry.Phrase, Source: entry.Source})
	}
	return personaBundle{Version: personaBundleVersion, Persona: p.Input(), Phrases: phrases}, true
}

func (h *Handler) personaBundleExport(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	bundle, ok := h.personaBundle(r.URL.Query().Get("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": bundle.Persona.Name + ".json"}))
	w.Header().Set("Cache-Control", "no-store")
	h.writeJSON(w, http.StatusOK, bundle)
}

func (h *Handler) personaBundleCopy(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.personasFail(w, r, http.StatusBadRequest, "Could not read the copy request.", nil, "", nil)
		return
	}
	bundle, ok := h.personaBundle(r.PostFormValue("source"))
	if !ok {
		h.personasFail(w, r, http.StatusNotFound, "That persona does not exist.", nil, "", nil)
		return
	}
	bundle.Persona.Name = r.PostFormValue("name")
	h.savePersonaBundle(w, r, bundle, "Saved a copy")
}

func (h *Handler) personaBundleImport(w http.ResponseWriter, r *http.Request) {
	if !h.phrasebookReady(w, r) {
		return
	}
	if err := limitForm(w, r); err != nil {
		h.personasFail(w, r, http.StatusBadRequest, "Could not read the import. The upload limit is 1 MiB.", nil, "", nil)
		return
	}
	decoder := json.NewDecoder(strings.NewReader(r.PostFormValue("bundle")))
	decoder.DisallowUnknownFields()
	var bundle personaBundle
	if err := decoder.Decode(&bundle); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF || bundle.Version != personaBundleVersion || bundle.Phrases == nil {
		h.personasFail(w, r, http.StatusBadRequest, "Choose a valid persona and common phrases export (version 1).", nil, "", nil)
		return
	}
	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
		bundle.Persona.Name = name
	}
	h.savePersonaBundle(w, r, bundle, "Imported")
}

func (h *Handler) savePersonaBundle(w http.ResponseWriter, r *http.Request, bundle personaBundle, action string) {
	p, err := h.personas.Import(r.Context(), bundle.Persona, h.phrases, bundle.Phrases)
	if err != nil {
		status, message := personaErrorStatus(err)
		if errors.Is(err, phrasebook.ErrInvalidPhrase) || errors.Is(err, phrasebook.ErrUnknownTool) {
			status, message = http.StatusBadRequest, "Each common phrase needs a unique, supported Tool, a valid source, and one line of up to 80 characters."
		}
		h.logPersonaError("save bundle", err)
		h.personasFail(w, r, status, message, nil, "", nil)
		return
	}
	h.writeJSON(w, http.StatusCreated, h.buildPersonasView(action+" as "+p.Name+".", "", nil, "", nil))
}
