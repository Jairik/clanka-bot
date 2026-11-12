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

func (f *personasFixture) get(target string) *httptest.ResponseRecorder {
	f.t.Helper()
	return serve(f.handler, request(http.MethodGet, target, owner))
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

func TestPersonasPageRequiresIdentityAndListsTheLibrary(t *testing.T) {
	f := newPersonasFixture(t)
	if rec := serve(f.handler, request(http.MethodGet, "/personas", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("no identity status = %d, want 403", rec.Code)
	}
	rec := f.get("/personas")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /personas = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Personas", `href="/personas" aria-current="page"`,
		"No persona is selected", `action="/personas"`,
		`name="personality"`, `name="voice"`, "Create persona",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// The nav keeps every existing page link.
	for _, link := range []string{"/health", "/sounds", "/voices", "/secrets", "/settings"} {
		if !strings.Contains(body, `href="`+link+`"`) {
			t.Errorf("nav lost %s", link)
		}
	}
}

func TestPersonaCreateEditSelectAndResetJourney(t *testing.T) {
	f := newPersonasFixture(t)
	rec := f.post("/personas", personaValues("GLaDOS", "jarvis", "You are GLaDOS: dry and precise."))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/personas?flash=persona-created" {
		t.Fatalf("create = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := f.personas.Get("glados"); !ok {
		t.Fatal("create did not persist GLaDOS")
	}
	// Creating a persona must not change the speaking voice or wording.
	if got := f.settings.String(voicelib.KeyName); got != "alba" {
		t.Fatalf("voice after create = %q, want alba", got)
	}

	page := f.get("/personas?flash=persona-created").Body.String()
	for _, want := range []string{"Persona created", "GLaDOS", "jarvis", "Select this persona"} {
		if !strings.Contains(page, want) {
			t.Errorf("page after create is missing %q", want)
		}
	}

	// Edit keeps the name and replaces the prompt.
	edited := personaValues("ignored", "alba", "You are GLaDOS, now polite.")
	rec = f.post("/personas/GLaDOS", edited)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := f.personas.Get("GLaDOS"); p.Voice != "alba" || !strings.Contains(p.Personality, "polite") {
		t.Fatalf("edited persona = %+v", p)
	}

	// Select applies the whole bundle, including the edited voice/wording.
	rec = f.post("/personas/GLaDOS/select", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/personas?flash=persona-selected" {
		t.Fatalf("select = %d %q", rec.Code, rec.Header().Get("Location"))
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
	activePage := f.get("/personas").Body.String()
	for _, want := range []string{"Active</span>", "Exact bundle", "Clear the active persona"} {
		if !strings.Contains(activePage, want) {
			t.Errorf("active page is missing %q", want)
		}
	}

	// Clearing removes only the marker; the applied bundle stays.
	rec = f.post("/personas/active/reset", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset = %d", rec.Code)
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
			body := rec.Body.String()
			if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, tc.want) {
				t.Fatalf("error body is missing the alert or %q:\n%s", tc.want, body)
			}
			// The rejected create form keeps what the user typed.
			if value := tc.values.Get("personality"); value != "" && !strings.Contains(body, htmlEscape(value)) {
				t.Errorf("rejected input %q was not preserved escaped", value)
			}
			if len(f.personas.List()) != 0 {
				t.Fatal("a rejected create wrote a row")
			}
		})
	}

	if rec := f.post("/personas", personaValues("GLaDOS", "alba", "You are GLaDOS.")); rec.Code != http.StatusSeeOther {
		t.Fatalf("valid create = %d", rec.Code)
	}
	rec := f.post("/personas", personaValues("glados", "jarvis", "Duplicate."))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("duplicate body has no safe message:\n%s", rec.Body.String())
	}
}

func TestPersonaPageEscapesHostileValues(t *testing.T) {
	f := newPersonasFixture(t)
	// A slash is rejected in a name, so the hostile name uses angle brackets
	// only; the prompt exercises the script payload.
	hostile := `<script>alert("x")</script>`
	values := personaValues("<b>evil", "alba", hostile)
	if rec := f.post("/personas", values); rec.Code != http.StatusSeeOther {
		t.Fatalf("create hostile = %d %s", rec.Code, rec.Body.String())
	}
	body := f.get("/personas").Body.String()
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>evil") {
		t.Fatal("hostile persona content rendered unescaped")
	}
	for _, want := range []string{"&lt;script&gt;alert", "&lt;b&gt;evil"} {
		if !strings.Contains(body, want) {
			t.Errorf("escaped content is missing %q", want)
		}
	}
}

func TestPersonaDriftIsReportedAfterAManualOverride(t *testing.T) {
	f := newPersonasFixture(t)
	if rec := f.post("/personas", personaValues("GLaDOS", "jarvis", "You are GLaDOS.")); rec.Code != http.StatusSeeOther {
		t.Fatalf("create = %d", rec.Code)
	}
	if rec := f.post("/personas/GLaDOS/select", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("select = %d", rec.Code)
	}
	if _, err := f.settings.Set(f.ctx, voicelib.KeyName, "alba"); err != nil {
		t.Fatalf("override voice: %v", err)
	}
	body := f.get("/personas").Body.String()
	for _, want := range []string{
		"The voice is set to alba instead of jarvis.",
		"Select the persona again to apply its stored voice and wording.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("drift page is missing %q", want)
		}
	}
}

func TestPersonaFormsRequireSameOriginAndWorkAsHTMXFragments(t *testing.T) {
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

	// An htmx create swaps the content fragment, not a whole document.
	req = httptest.NewRequest(http.MethodPost, "/personas", strings.NewReader(personaValues("GLaDOS", "alba", "You are GLaDOS.").Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("HX-Request", "true")
	rec := serve(f.handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx create status = %d", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if strings.Contains(body, "<html") || !strings.HasPrefix(body, `<div id="personas-content"`) {
		t.Fatalf("htmx body is not the personas fragment:\n%.200s", body)
	}

	// An htmx validation error also returns a 200 fragment with the alert.
	req = httptest.NewRequest(http.MethodPost, "/personas", strings.NewReader(personaValues("", "alba", "").Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("HX-Request", "true")
	rec = serve(f.handler, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `role="alert"`) {
		t.Fatalf("htmx error = %d, want a 200 alert fragment", rec.Code)
	}
}

// htmlEscape mirrors html/template's escaping for the small characters used in
// the preserved-input assertion.
func htmlEscape(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;",
	)
	return replacer.Replace(text)
}
