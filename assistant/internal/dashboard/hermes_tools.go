package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Jairik/clanka-bot/assistant/internal/hermesadapter"
)

// hermesTools returns the owner-only manager view.
func (h *Handler) hermesTools(w http.ResponseWriter, r *http.Request) {
	view, err := h.hermesAdapter.RecipeTools(r.Context())
	h.writeHermesTools(w, view, err)
}

// hermesToolSave accepts exactly one bounded JSON recipe.
func (h *Handler) hermesToolSave(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	var recipe *hermesadapter.Recipe
	if err := decoder.Decode(&recipe); err != nil || recipe == nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send a single valid JSON recipe."})
		return
	}
	view, err := h.hermesAdapter.SaveRecipe(r.Context(), *recipe)
	h.writeHermesTools(w, view, err)
}

// hermesToolDelete removes the named recipe and returns the current view.
func (h *Handler) hermesToolDelete(w http.ResponseWriter, r *http.Request) {
	view, err := h.hermesAdapter.DeleteRecipe(r.Context(), r.PathValue("name"))
	h.writeHermesTools(w, view, err)
}

// writeHermesTools keeps storage errors out of the API response.
func (h *Handler) writeHermesTools(w http.ResponseWriter, view hermesadapter.RecipeTools, err error) {
	switch {
	case err == nil:
		h.writeJSON(w, http.StatusOK, view)
	case errors.Is(err, hermesadapter.ErrInvalidRecipe):
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, hermesadapter.ErrRecipeNotFound):
		h.writeJSON(w, http.StatusNotFound, map[string]string{"error": "That recipe does not exist."})
	case errors.Is(err, hermesadapter.ErrRecipesUnavailable):
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The Hermes tool manager is unavailable."})
	default:
		h.logger.Error("Hermes tool manager storage failure", "error", err.Error())
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not access Hermes tool recipes."})
	}
}
