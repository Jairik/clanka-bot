package dashboard_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	listcore "github.com/Jairik/clanka-bot/assistant/tools/list"
	notecore "github.com/Jairik/clanka-bot/assistant/tools/note"
)

type notesFixture struct {
	t       *testing.T
	store   *store.Store
	notes   *notecore.Manager
	lists   *listcore.Manager
	handler *dashboard.Handler
}

func newNotesFixture(t *testing.T, withModules bool) *notesFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := &notesFixture{t: t, store: st}
	deps := tool.Deps{
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) },
	}
	modules := alertModules{}
	noteModule := notecore.NewModule()
	listModule := listcore.NewModule()
	if err := noteModule.Register(tool.NewRegistry(time.Second), deps); err != nil {
		t.Fatalf("register note module: %v", err)
	}
	if err := listModule.Register(tool.NewRegistry(time.Second), deps); err != nil {
		t.Fatalf("register list module: %v", err)
	}
	f.notes = noteModule.Manager()
	f.lists = listModule.Manager()
	if f.notes == nil || f.lists == nil {
		t.Fatal("a Notes manager is nil after Register")
	}
	modules[noteModule.Name()] = noteModule
	modules[listModule.Name()] = listModule

	opts := dashboard.Options{Now: func() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) }}
	if withModules {
		opts.ToolModules = modules
	}
	f.handler = newHandler(t, opts)
	return f
}

// notesViewJSON mirrors the Go view for assertions without importing the
// internal type.
type notesViewJSON struct {
	Flash    string
	Error    string
	HasStore bool
	Lists    []struct {
		Key       string
		Title     string
		Open      int
		AddAction string
		Items     []struct {
			ID           string
			Text         string
			Done         bool
			ToggleAction string
			EditAction   string
			RemoveAction string
		}
	}
	Notes []struct {
		ID           string
		Text         string
		Updated      string
		UpdatedISO   string
		EditAction   string
		DeleteAction string
	}
}

func (f *notesFixture) get() notesViewJSON {
	f.t.Helper()
	rec := serve(f.handler, request(http.MethodGet, "/api/ui/notes", owner))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET notes status = %d: %s", rec.Code, rec.Body.String())
	}
	var v notesViewJSON
	decodeJSON(f.t, rec, &v)
	return v
}

func (f *notesFixture) post(target string, values url.Values) (notesViewJSON, *httptest.ResponseRecorder) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	var v notesViewJSON
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		decodeJSON(f.t, rec, &v)
	}
	return v, rec
}

func TestNotesPageWithoutManagersIsReadOnly(t *testing.T) {
	f := newNotesFixture(t, false)
	v := f.get()
	if v.HasStore {
		t.Fatal("HasStore = true without a module list")
	}
	if len(v.Lists) != 0 || len(v.Notes) != 0 {
		t.Fatalf("view = %+v, want empty", v)
	}
}

func TestNotesPageListsBothFixedListsAndNotes(t *testing.T) {
	f := newNotesFixture(t, true)
	ctx := context.Background()
	if _, _, err := f.lists.Add(ctx, listcore.KeyShopping, "milk"); err != nil {
		t.Fatalf("Add shopping: %v", err)
	}
	if _, _, err := f.lists.Add(ctx, listcore.KeyTodo, "call the dentist"); err != nil {
		t.Fatalf("Add todo: %v", err)
	}
	if _, err := f.notes.Remember(ctx, "the spare key is under the pot"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	v := f.get()
	if !v.HasStore {
		t.Fatal("HasStore = false with both modules wired")
	}
	if len(v.Lists) != 2 || v.Lists[0].Key != listcore.KeyShopping || v.Lists[1].Key != listcore.KeyTodo {
		t.Fatalf("lists = %+v, want shopping then to-do", v.Lists)
	}
	if len(v.Lists[0].Items) != 1 || v.Lists[0].Items[0].Text != "milk" || v.Lists[0].Open != 1 {
		t.Fatalf("shopping = %+v", v.Lists[0])
	}
	if len(v.Notes) != 1 || v.Notes[0].Text != "the spare key is under the pot" {
		t.Fatalf("notes = %+v", v.Notes)
	}
	if v.Lists[0].AddAction != "/notes/lists/shopping/items" || v.Notes[0].DeleteAction != "/notes/1/delete" {
		t.Fatalf("actions = %q / %q", v.Lists[0].AddAction, v.Notes[0].DeleteAction)
	}
}

func TestNotesWritesAddEditTickRemoveAndDelete(t *testing.T) {
	f := newNotesFixture(t, true)

	v, rec := f.post("/notes", url.Values{"text": {"the boiler needs servicing"}})
	if rec.Code != http.StatusOK || v.Flash == "" || len(v.Notes) != 1 {
		t.Fatalf("create note = %d, %+v", rec.Code, v)
	}
	noteID := v.Notes[0].ID

	v, rec = f.post("/notes/"+noteID, url.Values{"text": {"the boiler is serviced on Tuesday"}})
	if rec.Code != http.StatusOK || v.Notes[0].Text != "the boiler is serviced on Tuesday" {
		t.Fatalf("update note = %d, %+v", rec.Code, v)
	}

	v, rec = f.post("/notes/lists/shopping/items", url.Values{"text": {"milk"}})
	if rec.Code != http.StatusOK || len(v.Lists[0].Items) != 1 {
		t.Fatalf("add item = %d, %+v", rec.Code, v)
	}
	itemID := v.Lists[0].Items[0].ID

	v, rec = f.post("/notes/lists/shopping/items/"+itemID+"/toggle", nil)
	if rec.Code != http.StatusOK || !v.Lists[0].Items[0].Done || v.Lists[0].Open != 0 {
		t.Fatalf("toggle = %d, %+v", rec.Code, v)
	}

	v, rec = f.post("/notes/lists/shopping/items/"+itemID, url.Values{"text": {"oat milk"}})
	if rec.Code != http.StatusOK || v.Lists[0].Items[0].Text != "oat milk" {
		t.Fatalf("update item = %d, %+v", rec.Code, v)
	}

	v, rec = f.post("/notes/lists/shopping/items/"+itemID+"/remove", nil)
	if rec.Code != http.StatusOK || len(v.Lists[0].Items) != 0 {
		t.Fatalf("remove item = %d, %+v", rec.Code, v)
	}

	v, rec = f.post("/notes/"+noteID+"/delete", nil)
	if rec.Code != http.StatusOK || len(v.Notes) != 0 {
		t.Fatalf("delete note = %d, %+v", rec.Code, v)
	}
}

func TestNotesWritesRejectBadInputAndWrongLists(t *testing.T) {
	f := newNotesFixture(t, true)

	for _, tc := range []struct {
		target string
		values url.Values
		status int
	}{
		{"/notes", url.Values{"text": {"   "}}, http.StatusBadRequest},
		{"/notes/lists/shopping/items", url.Values{"text": {" "}}, http.StatusBadRequest},
		{"/notes/lists/wish/items", url.Values{"text": {"milk"}}, http.StatusNotFound},
		{"/notes/999/delete", nil, http.StatusOK},
	} {
		t.Run(tc.target, func(t *testing.T) {
			_, rec := f.post(tc.target, tc.values)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}

	// A duplicate open item is refused rather than added twice.
	if _, rec := f.post("/notes/lists/shopping/items", url.Values{"text": {"milk"}}); rec.Code != http.StatusOK {
		t.Fatalf("first add = %d", rec.Code)
	}
	if _, rec := f.post("/notes/lists/shopping/items", url.Values{"text": {"MILK"}}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate add = %d, want 409", rec.Code)
	}
}

func TestNotesWritesRequireIdentityAndSameOrigin(t *testing.T) {
	f := newNotesFixture(t, true)

	req := httptest.NewRequest(http.MethodPost, "/notes", strings.NewReader(url.Values{"text": {"x"}}.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := serve(f.handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing login status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/notes", strings.NewReader(url.Values{"text": {"x"}}.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = serve(f.handler, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}
}

func TestNotesRoundTripHostileTextWithoutRawMarkup(t *testing.T) {
	f := newNotesFixture(t, true)
	hostile := `Read <script>alert(1)</script> "now" & later`
	_, rec := f.post("/notes", url.Values{"text": {hostile}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	noRawScript(t, rec.Body.String())
	v := f.get()
	if len(v.Notes) != 1 || v.Notes[0].Text != hostile {
		t.Fatalf("notes = %+v, want the hostile text unchanged", v.Notes)
	}
}
