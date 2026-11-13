package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	listcore "github.com/Jairik/clanka-bot/assistant/tools/list"
	notecore "github.com/Jairik/clanka-bot/assistant/tools/note"
)

// Notes page flash values. They are internal names, never user text.
const (
	flashNoteSaved   = "note-saved"
	flashNoteUpdated = "note-updated"
	flashNoteDeleted = "note-deleted"
	flashItemAdded   = "item-added"
	flashItemToggled = "item-toggled"
	flashItemUpdated = "item-updated"
	flashItemRemoved = "item-removed"
	flashNoteGone    = "note-gone"
	flashItemGone    = "item-gone"
)

// notesView is the Notes page data model: the two fixed Lists and every Note.
type notesView struct {
	Owner    string
	Page     string
	Flash    string
	Error    string
	HasStore bool
	Lists    []notesListView
	Notes    []noteRowView
}

// notesListView is one fixed list with its rows and add action.
type notesListView struct {
	Key       string
	Title     string
	Open      int
	Items     []noteItemView
	AddAction string
}

// noteItemView is one list row with its three write actions.
type noteItemView struct {
	ID           string
	Text         string
	Done         bool
	ToggleAction string
	EditAction   string
	RemoveAction string
}

// noteRowView is one Note with its edit and delete actions.
type noteRowView struct {
	ID           string
	Text         string
	Updated      string
	UpdatedISO   string
	EditAction   string
	DeleteAction string
}

// notesDeps is the typed set of managers one Notes request uses.
type notesDeps struct {
	notes *notecore.Manager
	lists *listcore.Manager
}

// notesPage renders both fixed lists and the notes.
func (h *Handler) notesPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildNotesView(notesFlashMessage(r.URL.Query().Get("flash")), ""))
}

// noteCreate stores one note.
func (h *Handler) noteCreate(w http.ResponseWriter, r *http.Request) {
	manager := h.noteManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.notesFail(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	note, err := manager.Remember(r.Context(), r.FormValue("text"))
	if err != nil {
		h.notesWriteError(w, "create note", err)
		return
	}
	h.logger.Info("note created from dashboard", "note_id", note.ID)
	h.notesSuccess(w, flashNoteSaved)
}

// noteUpdate replaces one note's text.
func (h *Handler) noteUpdate(w http.ResponseWriter, r *http.Request) {
	manager := h.noteManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.notesFail(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	if _, err := manager.Update(r.Context(), id, r.FormValue("text")); err != nil {
		h.notesWriteError(w, "update note", err)
		return
	}
	h.logger.Info("note updated from dashboard", "note_id", id)
	h.notesSuccess(w, flashNoteUpdated)
}

// noteDelete removes one note.
func (h *Handler) noteDelete(w http.ResponseWriter, r *http.Request) {
	manager := h.noteManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	deleted, err := manager.Delete(r.Context(), id)
	if err != nil {
		h.notesWriteError(w, "delete note", err)
		return
	}
	if deleted == 0 {
		h.notesSuccess(w, flashNoteGone)
		return
	}
	h.logger.Info("note deleted from dashboard", "note_id", id)
	h.notesSuccess(w, flashNoteDeleted)
}

// listItemAdd stores one item on a fixed list.
func (h *Handler) listItemAdd(w http.ResponseWriter, r *http.Request) {
	manager := h.listManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	key, ok := fixedListKey(r.PathValue("list"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.notesFail(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	item, added, err := manager.Add(r.Context(), key, r.FormValue("text"))
	if err != nil {
		h.notesWriteError(w, "add item", err)
		return
	}
	if !added {
		h.notesFail(w, http.StatusConflict, "That item is already on the list.")
		return
	}
	h.logger.Info("list item added from dashboard", "list", key, "item_id", item.ID)
	h.notesSuccess(w, flashItemAdded)
}

// listItemToggle ticks or unticks one row.
func (h *Handler) listItemToggle(w http.ResponseWriter, r *http.Request) {
	manager := h.listManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	key, ok := fixedListKey(r.PathValue("list"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	items, err := manager.Items(r.Context(), key)
	if err != nil {
		h.notesWriteError(w, "read list", err)
		return
	}
	done := false
	found := false
	for _, item := range items {
		if item.ID == id {
			done = !item.Done
			found = true
			break
		}
	}
	if !found {
		h.notesSuccess(w, flashItemGone)
		return
	}
	if _, err := manager.SetDone(r.Context(), key, id, done); err != nil {
		h.notesWriteError(w, "tick item", err)
		return
	}
	h.logger.Info("list item toggled from dashboard", "list", key, "item_id", id, "done", done)
	h.notesSuccess(w, flashItemToggled)
}

// listItemUpdate replaces one row's text.
func (h *Handler) listItemUpdate(w http.ResponseWriter, r *http.Request) {
	manager := h.listManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	key, ok := fixedListKey(r.PathValue("list"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := limitForm(w, r); err != nil {
		h.notesFail(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	if _, err := manager.UpdateItem(r.Context(), key, id, r.FormValue("text")); err != nil {
		h.notesWriteError(w, "update item", err)
		return
	}
	h.logger.Info("list item updated from dashboard", "list", key, "item_id", id)
	h.notesSuccess(w, flashItemUpdated)
}

// listItemRemove deletes one row.
func (h *Handler) listItemRemove(w http.ResponseWriter, r *http.Request) {
	manager := h.listManager()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	key, ok := fixedListKey(r.PathValue("list"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := manager.RemoveByID(r.Context(), key, id); err != nil {
		if errors.Is(err, listcore.ErrItemNotFound) {
			h.notesSuccess(w, flashItemGone)
			return
		}
		h.notesWriteError(w, "remove item", err)
		return
	}
	h.logger.Info("list item removed from dashboard", "list", key, "item_id", id)
	h.notesSuccess(w, flashItemRemoved)
}

// notesSuccess answers with the refreshed view, so the app can replace the
// page state from one response.
func (h *Handler) notesSuccess(w http.ResponseWriter, flash string) {
	h.writeJSON(w, http.StatusOK, h.buildNotesView(notesFlashMessage(flash), ""))
}

// notesFail answers with an error and the unchanged view.
func (h *Handler) notesFail(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, h.buildNotesView("", message))
}

// notesWriteError maps a manager error to the page: a spoken validation error
// is safe to show, a missing row is a 404 and anything else stays generic.
func (h *Handler) notesWriteError(w http.ResponseWriter, operation string, err error) {
	var spoken *notecore.Error
	if errors.As(err, &spoken) {
		h.notesFail(w, http.StatusBadRequest, spoken.Message)
		return
	}
	var listError *listcore.Error
	if errors.As(err, &listError) {
		h.notesFail(w, http.StatusBadRequest, listError.Message)
		return
	}
	switch {
	case errors.Is(err, notecore.ErrUnavailable), errors.Is(err, listcore.ErrUnavailable):
		h.notesFail(w, http.StatusServiceUnavailable, "The note store is not available.")
	case errors.Is(err, listcore.ErrItemNotFound):
		h.notesFail(w, http.StatusNotFound, "That list item was already gone.")
	default:
		h.logger.Error("notes write", "operation", operation, "error", err.Error())
		h.notesFail(w, http.StatusInternalServerError, "Could not save that change.")
	}
}

// buildNotesView resolves the two managers and formats every row.
func (h *Handler) buildNotesView(flash, errorText string) notesView {
	view := notesView{Owner: h.owner, Page: "notes", Flash: flash, Error: errorText}
	deps := h.notesDeps()
	if deps.lists == nil && deps.notes == nil {
		return view
	}
	view.HasStore = true
	now := h.now()
	loc := h.alertLocation()
	ctx := context.Background()
	if deps.lists != nil {
		lists, err := deps.lists.All(ctx)
		if err != nil {
			h.logger.Warn("notes list read failed", "error", err.Error())
		}
		for _, list := range lists {
			row := notesListView{
				Key:       list.Key,
				Title:     list.Title,
				AddAction: "/notes/lists/" + url.PathEscape(list.Key) + "/items",
			}
			for _, item := range list.Items {
				if !item.Done {
					row.Open++
				}
				row.Items = append(row.Items, noteItemView{
					ID:           item.ID,
					Text:         item.Text,
					Done:         item.Done,
					ToggleAction: "/notes/lists/" + url.PathEscape(list.Key) + "/items/" + url.PathEscape(item.ID) + "/toggle",
					EditAction:   "/notes/lists/" + url.PathEscape(list.Key) + "/items/" + url.PathEscape(item.ID),
					RemoveAction: "/notes/lists/" + url.PathEscape(list.Key) + "/items/" + url.PathEscape(item.ID) + "/remove",
				})
			}
			view.Lists = append(view.Lists, row)
		}
	}
	if deps.notes != nil {
		notes, err := deps.notes.List(ctx)
		if err != nil {
			h.logger.Warn("notes read failed", "error", err.Error())
		}
		for _, note := range notes {
			view.Notes = append(view.Notes, noteRowView{
				ID:           note.ID,
				Text:         note.Text,
				Updated:      formatAlertTime(note.UpdatedAt, now, loc),
				UpdatedISO:   note.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
				EditAction:   "/notes/" + url.PathEscape(note.ID),
				DeleteAction: "/notes/" + url.PathEscape(note.ID) + "/delete",
			})
		}
	}
	return view
}

// notesDeps resolves the two managers once from the one module list.
func (h *Handler) notesDeps() notesDeps {
	return notesDeps{notes: h.noteManager(), lists: h.listManager()}
}

// noteManager and listManager are the one module-list lookup behind notesDeps.
func (h *Handler) noteManager() *notecore.Manager {
	if h.toolModules == nil {
		return nil
	}
	module, _ := h.toolModules.Module("note").(*notecore.Module)
	if module == nil {
		return nil
	}
	return module.Manager()
}

func (h *Handler) listManager() *listcore.Manager {
	if h.toolModules == nil {
		return nil
	}
	module, _ := h.toolModules.Module("list").(*listcore.Module)
	if module == nil {
		return nil
	}
	return module.Manager()
}

// fixedListKey accepts only the two shipped list keys from the URL.
func fixedListKey(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case listcore.KeyShopping:
		return listcore.KeyShopping, true
	case listcore.KeyTodo:
		return listcore.KeyTodo, true
	default:
		return "", false
	}
}

// notesFlashMessage maps the closed flash set to the user-visible result.
func notesFlashMessage(flash string) string {
	switch flash {
	case flashNoteSaved:
		return "Note saved."
	case flashNoteUpdated:
		return "Note updated."
	case flashNoteDeleted:
		return "Note deleted."
	case flashNoteGone:
		return "That note was already gone."
	case flashItemAdded:
		return "Added to the list."
	case flashItemToggled:
		return "List updated."
	case flashItemUpdated:
		return "List item updated."
	case flashItemRemoved:
		return "Removed from the list."
	case flashItemGone:
		return "That list item was already gone."
	default:
		return ""
	}
}
