package dashboard_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

type personasFixture struct {
	t        *testing.T
	ctx      context.Context
	handler  *dashboard.Handler
	settings *settings.Service
	personas *persona.Service
}

// newPersonasFixture wires the real Persona and settings services over a real
// SQLite database and a real voice library with built-in states only.
func newPersonasFixture(t *testing.T) *personasFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	voices, err := voicelib.New(ctx, st, voicelib.Config{
		Dir:      filepath.Join(dir, "voices"),
		TTSDir:   "/voices",
		Default:  "alba",
		Builtins: []string{"alba", "jarvis"},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	personas, err := persona.New(ctx, st, voices, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("persona.New: %v", err)
	}
	defs := []settings.Definition{
		settings.String(voicelib.KeyName, "alba", settings.WithConfig("alba")),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig(wording.DefaultFiller)),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage, settings.WithConfig(wording.DefaultUnavailableMessage)),
	}
	defs = append(defs, personas.Definitions()...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	voices.Bind(svc)
	personas.Bind(svc)
	handler := newHandler(t, dashboard.Options{
		Settings: svc,
		Voices:   voices,
		Personas: personas,
	})
	return &personasFixture{t: t, ctx: ctx, handler: handler, settings: svc, personas: personas}
}

// personasResp decodes the Persona view the tests assert on (encoding/json
// default field names).
type personasResp struct {
	Flash      string
	Error      string
	HasService bool
	Active     *struct {
		Name, Voice, EffectiveVoice string
		Exact                       bool
		Notes                       []string
	}
	New  personaForm
	Rows []struct {
		Name, Voice, VoiceStatus string
		Active                   bool
		SelectAction             string
		Edit                     personaForm
	}
}

type personaForm struct {
	Action, Name, Voice, Personality, Filler, Unavailable string
	NameLocked                                            bool
	Voices                                                []struct {
		Name, Label string
		Selected    bool
	}
}

func (f *personasFixture) get(target string) (personasResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	var v personasResp
	rec := getJSON(f.t, f.handler, target, &v)
	return v, rec
}

func (f *personasFixture) view() personasResp {
	f.t.Helper()
	v, rec := f.get("/api/ui/personas")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET personas = %d: %s", rec.Code, rec.Body.String())
	}
	return v
}

// postView posts a form and decodes the answered view.
func (f *personasFixture) postView(target string, values url.Values) (personasResp, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.post(target, values)
	var v personasResp
	decodeJSON(f.t, rec, &v)
	return v, rec
}

func (f *personasFixture) post(target string, values url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// personaValues builds one valid create/edit form.
func personaValues(name, voice, personality string) url.Values {
	return url.Values{
		"name":        {name},
		"voice":       {voice},
		"personality": {personality},
		"filler":      {"Processing."},
		"unavailable": {"The chamber is offline."},
	}
}

func TestPersonasReadRequiresIdentityAndListsTheLibrary(t *testing.T) {
	f := newPersonasFixture(t)
	if rec := serve(f.handler, request(http.MethodGet, "/api/ui/personas", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("no identity status = %d, want 403", rec.Code)
	}
	v, rec := f.get("/api/ui/personas")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET personas = %d", rec.Code)
	}
	if !v.HasService || v.Active != nil || len(v.Rows) != 0 {
		t.Errorf("empty library view = %+v, want a service with no active persona", v)
	}
	if v.New.Action != "/personas" || len(v.New.Voices) == 0 {
		t.Errorf("create form = %+v, want the create action and a voice list", v.New)
	}
	if v.New.Voice != "alba" || v.New.Filler != wording.DefaultFiller {
		t.Errorf("create form starts from voice %q filler %q, want the live values", v.New.Voice, v.New.Filler)
	}
}

func TestPersonaCreateEditSelectAndResetJourney(t *testing.T) {
	f := newPersonasFixture(t)
	created, rec := f.postView("/personas", personaValues("GLaDOS", "jarvis", "You are GLaDOS: dry and precise."))
	if rec.Code != http.StatusOK || !strings.Contains(created.Flash, "Persona created") {
		t.Fatalf("create = %d flash %q", rec.Code, created.Flash)
	}
	if _, ok := f.personas.Get("glados"); !ok {
		t.Fatal("create did not persist GLaDOS")
	}
	// Creating a persona must not change the speaking voice or wording.
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("voice after create = %q, want alba", got)
	}

	page, _ := f.get("/api/ui/personas?flash=persona-created")
	if !strings.Contains(page.Flash, "Persona created") {
		t.Errorf("?flash= read lacks the notice: %q", page.Flash)
	}
	if len(page.Rows) != 1 || page.Rows[0].Name != "GLaDOS" || page.Rows[0].Voice != "jarvis" ||
		page.Rows[0].SelectAction != "/personas/GLaDOS/select" || page.Rows[0].Active {
		t.Errorf("rows after create = %+v", page.Rows)
	}

	// Edit keeps the name and replaces the prompt.
	edited := personaValues("ignored", "alba", "You are GLaDOS, now polite.")
	rec = f.post("/personas/GLaDOS", edited)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := f.personas.Get("GLaDOS"); p.Voice != "alba" || !strings.Contains(p.Personality, "polite") {
		t.Fatalf("edited persona = %+v", p)
	}

	// Select applies the whole bundle, including the edited voice/wording.
	selected, rec := f.postView("/personas/GLaDOS/select", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(selected.Flash, "Selected.") {
		t.Fatalf("select = %d flash %q", rec.Code, selected.Flash)
	}
	values := f.settings.GetMany(voicelib.KeyName, wording.FillerKey, wording.UnavailableKey, persona.ActiveKey)
	for key, want := range map[string]string{
		voicelib.KeyName:       "alba",
		wording.FillerKey:      "Processing.",
		wording.UnavailableKey: "The chamber is offline.",
		persona.ActiveKey:      "GLaDOS",
	} {
		if got := values[key].Value; got != want {
			t.Errorf("%s = %v, want %q", key, got, want)
		}
	}
	active := f.view()
	if active.Active == nil || active.Active.Name != "GLaDOS" || !active.Active.Exact || len(active.Active.Notes) != 0 {
		t.Errorf("active summary = %+v, want the exact GLaDOS bundle", active.Active)
	}
	if len(active.Rows) != 1 || !active.Rows[0].Active {
		t.Errorf("rows = %+v, want GLaDOS marked active", active.Rows)
	}

	// Clearing removes only the marker; the applied bundle stays.
	cleared, rec := f.postView("/personas/active/reset", nil)
	if rec.Code != http.StatusOK || cleared.Active != nil {
		t.Fatalf("reset = %d active %+v", rec.Code, cleared.Active)
	}
	if got := f.settings.String(persona.ActiveKey); got != "" {
		t.Fatalf("persona.active = %q after reset", got)
	}
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("voice after reset = %q, want the applied voice kept", got)
	}
	if got := f.settings.String(wording.FillerKey); got != "Processing." {
		t.Fatalf("filler after reset = %q, want the applied wording kept", got)
	}
}

func TestPersonaValidationRejectsSafelyAndPreservesInput(t *testing.T) {
	f := newPersonasFixture(t)
	cases := []struct {
		name   string
		values url.Values
		status int
		want   string
	}{
		{
			name:   "blank personality",
			values: url.Values{"name": {"GLaDOS"}, "voice": {"alba"}, "personality": {"  "}, "filler": {"Hi."}, "unavailable": {"Off."}},
			status: http.StatusBadRequest,
			want:   "Write the personality prompt.",
		},
		{
			name:   "unknown voice",
			values: url.Values{"name": {"GLaDOS"}, "voice": {"invented"}, "personality": {"You are GLaDOS."}, "filler": {"Hi."}, "unavailable": {"Off."}},
			status: http.StatusBadRequest,
			want:   "in the library",
		},
		{
			name:   "multi-line filler",
			values: url.Values{"name": {"GLaDOS"}, "voice": {"alba"}, "personality": {"You are GLaDOS."}, "filler": {"one\ntwo"}, "unavailable": {"Off."}},
			status: http.StatusBadRequest,
			want:   "must be one line",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.post("/personas", tc.values)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			var v personasResp
			decodeJSON(t, rec, &v)
			if !strings.Contains(v.Error, tc.want) {
				t.Fatalf("error %q is missing %q", v.Error, tc.want)
			}
			// The rejected create form keeps what the user typed, verbatim.
			if v.New.Name != tc.values.Get("name") || v.New.Personality != tc.values.Get("personality") ||
				v.New.Voice != tc.values.Get("voice") || v.New.Filler != tc.values.Get("filler") {
				t.Errorf("rejected create form = %+v, want the submitted input", v.New)
			}
			if len(f.personas.List()) != 0 {
				t.Fatal("a rejected create wrote a row")
			}
		})
	}

	if rec := f.post("/personas", personaValues("GLaDOS", "alba", "You are GLaDOS.")); rec.Code != http.StatusOK {
		t.Fatalf("valid create = %d", rec.Code)
	}
	dup, rec := f.postView("/personas", personaValues("glados", "jarvis", "Duplicate."))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", rec.Code)
	}
	if !strings.Contains(dup.Error, "already exists") {
		t.Fatalf("duplicate error has no safe message: %q", dup.Error)
	}
}

func TestPersonaHostileValuesRoundTripInertly(t *testing.T) {
	f := newPersonasFixture(t)
	// A slash is rejected in a name, so the hostile name uses angle brackets
	// only; the prompt exercises the script payload.
	hostile := `<script>alert("x")</script>`
	values := personaValues("<b>evil", "alba", hostile)
	if rec := f.post("/personas", values); rec.Code != http.StatusOK {
		t.Fatalf("create hostile = %d %s", rec.Code, rec.Body.String())
	}
	rec := serve(f.handler, request(http.MethodGet, "/api/ui/personas", owner))
	var v personasResp
	decodeJSON(t, rec, &v)
	if len(v.Rows) != 1 || v.Rows[0].Name != "<b>evil" || v.Rows[0].Edit.Personality != hostile {
		t.Fatalf("hostile persona did not round-trip exactly: %+v", v.Rows)
	}
	body := rec.Body.String()
	noRawScript(t, body)
	if strings.Contains(body, "<b>") {
		t.Error("hostile persona name is a raw tag in the body")
	}
}

func TestPersonaDriftIsReportedAfterAManualOverride(t *testing.T) {
	f := newPersonasFixture(t)
	if rec := f.post("/personas", personaValues("GLaDOS", "jarvis", "You are GLaDOS.")); rec.Code != http.StatusOK {
		t.Fatalf("create = %d", rec.Code)
	}
	if rec := f.post("/personas/GLaDOS/select", nil); rec.Code != http.StatusOK {
		t.Fatalf("select = %d", rec.Code)
	}
	if _, err := f.settings.Set(f.ctx, voicelib.KeyName, "alba"); err != nil {
		t.Fatalf("override voice: %v", err)
	}
	v := f.view()
	if v.Active == nil || v.Active.Exact {
		t.Fatalf("active = %+v, want a drifted (not exact) persona", v.Active)
	}
	notes := strings.Join(v.Active.Notes, "\n")
	for _, want := range []string{
		"The voice is set to alba instead of jarvis.",
		"Select the persona again to apply its stored voice and wording.",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("drift notes lack %q: %q", want, notes)
		}
	}
}

func TestPersonaFormsRequireIdentityAndSameOrigin(t *testing.T) {
	f := newPersonasFixture(t)
	// No identity.
	req := httptest.NewRequest(http.MethodPost, "/personas", strings.NewReader(personaValues("X", "alba", "Prompt.").Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := serve(f.handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("no identity status = %d, want 403", rec.Code)
	}
	// Cross-origin.
	cross := httptest.NewRequest(http.MethodPost, "/personas", strings.NewReader(personaValues("X", "alba", "Prompt.").Encode()))
	cross.Host = "127.0.0.1:8082"
	cross.RemoteAddr = "127.0.0.1:12345"
	cross.Header.Set("Tailscale-User-Login", owner)
	cross.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	cross.Header.Set("Origin", "https://evil.example")
	if rec := serve(f.handler, cross); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}
	if len(f.personas.List()) != 0 {
		t.Fatal("a rejected request created a persona")
	}
}

// TestPersonaWritesAnswerJSONWithTheRealStatus pins the write contract the
// React app relies on: success is 200 with the refreshed view, a validation
// error is the view with the real 4xx status and an Error.
func TestPersonaWritesAnswerJSONWithTheRealStatus(t *testing.T) {
	f := newPersonasFixture(t)
	v, rec := f.postView("/personas", personaValues("GLaDOS", "alba", "You are GLaDOS."))
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("create status = %d", rec.Code)
	}
	if len(v.Rows) != 1 || v.Rows[0].Name != "GLaDOS" || v.Error != "" {
		t.Fatalf("create view = %+v", v)
	}

	v, rec = f.postView("/personas", personaValues("", "alba", ""))
	if rec.Code != http.StatusBadRequest || v.Error == "" {
		t.Fatalf("validation error = %d %q, want 400 with an error", rec.Code, v.Error)
	}
}
