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
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/integrations"
	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/phrasebook"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

type phraseFixture struct {
	handler  *dashboard.Handler
	personas *persona.Service
	phrases  *phrasebook.Service
	settings *settings.Service
	secrets  *secrets.Service
}

type fakePhraseGen struct {
	reply string
	err   error
}

func (f fakePhraseGen) Complete(context.Context, string, string) (string, error) {
	return f.reply, f.err
}

func newPhraseFixture(t *testing.T, gen phrasebook.Completer) *phraseFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	voices, err := voicelib.New(ctx, st, voicelib.Config{
		Dir:      filepath.Join(dir, "voices"),
		TTSDir:   "/voices",
		Default:  "alba",
		Builtins: []string{"alba", "jarvis"},
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("voicelib.New: %v", err)
	}
	personas, err := persona.New(ctx, st, voices, logger)
	if err != nil {
		t.Fatalf("persona.New: %v", err)
	}
	defs := []settings.Definition{
		settings.String(voicelib.KeyName, "alba", settings.WithConfig("alba")),
		settings.String(wording.FillerKey, wording.DefaultFiller, settings.WithConfig(wording.DefaultFiller)),
		settings.String(wording.UnavailableKey, wording.DefaultUnavailableMessage, settings.WithConfig(wording.DefaultUnavailableMessage)),
	}
	defs = append(defs, personas.Definitions()...)
	defs = append(defs, integrations.Definitions()...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	voices.Bind(svc)
	personas.Bind(svc)
	phrases, err := phrasebook.New(ctx, st, func() string { return svc.String(persona.ActiveKey) }, logger)
	if err != nil {
		t.Fatalf("phrasebook.New: %v", err)
	}
	if _, err := personas.Create(ctx, persona.Input{
		Name: "Scuzz", Voice: "alba",
		Personality: "Swears constantly.",
		Filler:      "Fucking hold on",
		Unavailable: "Brain is dead",
	}); err != nil {
		t.Fatalf("create persona: %v", err)
	}
	secretSvc, err := secrets.New(ctx, st.DB(), []string{"GITHUB_TOKEN", "GITHUB_NOTIFICATIONS_TOKEN", "OPENROUTER_API_KEY", "SPOTIFY_REFRESH_TOKEN", "NTFY_TOPIC", "NTFY_TOKEN", "WEATHERAPI_API_KEY", "MONSTER_EMAIL", "MONSTER_PASSWORD"})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	integrationsSvc := integrations.New(secretSvc, svc)
	handler := newHandler(t, dashboard.Options{
		Settings: svc,
		Voices:   voices,
		Personas: personas,
		Phrases:  phrases,
		PhraseSlots: []phrasebook.Slot{
			{Tool: "timer.set", Group: "timer", Description: "Set a timer.", Default: "Set."},
			{Tool: "lights.power", Group: "lights", Description: "Power a light.", Default: "Done."},
		},
		PhraseGen:    gen,
		Integrations: integrationsSvc,
	})
	return &phraseFixture{handler: handler, personas: personas, phrases: phrases, settings: svc, secrets: secretSvc}
}

type phraseResp struct {
	Flash         string
	Error         string
	HasService    bool
	HasGenerator  bool
	ActivePersona string
	Selected      string
	Personas      []struct {
		Name   string
		Active bool
		Count  int
	}
	Slots []struct {
		Tool, Group, Description, Default, Phrase, Source string
		Overridden                                        bool
	}
	Total, Overridden, Generated, Manual int
}

func TestPhrasebookPageDefaultsAndPersona(t *testing.T) {
	f := newPhraseFixture(t, fakePhraseGen{reply: `{"timer.set":"Timer's fucking set.","lights.power":"Light's on."}`})
	var view phraseResp
	rec := getJSON(t, f.handler, "/api/ui/phrasebook", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !view.HasService || !view.HasGenerator {
		t.Fatalf("flags = %+v", view)
	}
	if view.Total != 2 || view.Overridden != 0 {
		t.Fatalf("totals = %+v", view)
	}
	for _, slot := range view.Slots {
		if slot.Source != "default" || slot.Phrase != slot.Default {
			t.Errorf("slot %+v is not the default", slot)
		}
	}
	if view.Selected != "" {
		t.Fatalf("selected with no active persona = %q", view.Selected)
	}
}

func TestPhrasebookSaveResetAndClear(t *testing.T) {
	f := newPhraseFixture(t, fakePhraseGen{})
	form := url.Values{"tool": {"timer.set"}, "phrase": {"Timer is fucking set."}}
	rec := serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz", form))
	var view phraseResp
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK || view.Flash == "" {
		t.Fatalf("save status = %d, flash = %q", rec.Code, view.Flash)
	}
	if view.Overridden != 1 || view.Manual != 1 {
		t.Fatalf("after save = %+v", view)
	}
	if _, err := f.personas.Select(context.Background(), "Scuzz"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got := f.phrases.Render("timer.set", "Timer set."); got != "Timer is fucking set." {
		t.Fatalf("Render = %q", got)
	}

	// Reset one row.
	rec = serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz/reset", url.Values{"tool": {"timer.set"}}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK || view.Overridden != 0 {
		t.Fatalf("reset status = %d, view = %+v", rec.Code, view)
	}

	// Save both, then clear everything.
	for _, tool := range []string{"timer.set", "lights.power"} {
		serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz", url.Values{"tool": {tool}, "phrase": {"Done."}}))
	}
	rec = serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz/clear", url.Values{}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK || view.Overridden != 0 {
		t.Fatalf("clear status = %d, view = %+v", rec.Code, view)
	}
}

func TestPhrasebookGenerateAndFailure(t *testing.T) {
	f := newPhraseFixture(t, fakePhraseGen{reply: "```json\n{\"timer.set\":\"Timer's set, fucker.\",\"lights.power\":\"Lit.\"}\n```"})
	rec := serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz/generate", url.Values{}))
	var view phraseResp
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status = %d, error = %s", rec.Code, view.Error)
	}
	if view.Overridden != 2 || view.Generated != 2 {
		t.Fatalf("after generate = %+v", view)
	}
	if _, err := f.personas.Select(context.Background(), "Scuzz"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got := f.phrases.Render("timer.set", "x"); got != "Timer's set, fucker." {
		t.Fatalf("Render = %q", got)
	}

	// A provider error is a 502 with a safe message, never the upstream body.
	bad := newPhraseFixture(t, fakePhraseGen{err: context.DeadlineExceeded})
	rec = serve(bad.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz/generate", url.Values{}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failure status = %d", rec.Code)
	}
	if strings.Contains(view.Error, "context deadline") {
		t.Fatalf("error leaked provider text: %q", view.Error)
	}

	// No generator at all is a 503 and the page says so.
	none := newPhraseFixture(t, nil)
	rec = serve(none.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz/generate", url.Values{}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-generator status = %d", rec.Code)
	}
	if view.HasGenerator {
		t.Fatal("HasGenerator = true with no generator")
	}
}

func TestPhrasebookRejectsUnknownTool(t *testing.T) {
	f := newPhraseFixture(t, nil)
	rec := serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz", url.Values{"tool": {"weather.current"}, "phrase": {"Sunny."}}))
	var view phraseResp
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

type hermesResp struct {
	Flash           string
	Error           string
	HasRegistry     bool
	HasIntegrations bool
	Surface         []struct{ Name, Description, Permission string }
	SurfaceCount    int
	LocalOnlyCount  int
	ConfirmCount    int
	Granted         int
	Rows            []struct {
		ID, Name, Summary, Docs, Status, StatusKey string
		Hermes, CanGrant, Linkable                 bool
		Secrets                                    []struct {
			Name, Source, Last4 string
			Set, Optional       bool
		}
		Tools []string
	}
}

func TestHermesPageLinksGrantsAndUnlinks(t *testing.T) {
	f := newPhraseFixture(t, nil)
	var view hermesResp
	rec := getJSON(t, f.handler, "/api/ui/hermes", &view)
	if rec.Code != http.StatusOK || !view.HasIntegrations {
		t.Fatalf("status = %d, integrations = %v", rec.Code, view.HasIntegrations)
	}
	if len(view.Rows) != len(integrations.Catalog) {
		t.Fatalf("accounts = %d, want %d", len(view.Rows), len(integrations.Catalog))
	}
	if view.Rows[0].Status != "Not linked" {
		t.Fatalf("initial status = %q", view.Rows[0].Status)
	}

	// Link GitHub with only its required token.
	rec = serve(f.handler, formRequest(http.MethodPost, "/hermes/integrations/github", url.Values{"GITHUB_TOKEN": {"ghp_dashboard_test"}}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("link status = %d, error = %s", rec.Code, view.Error)
	}
	if view.Rows[0].StatusKey != "linked" {
		t.Fatalf("linked status = %q", view.Rows[0].StatusKey)
	}

	// Grant and revoke.
	rec = serve(f.handler, formRequest(http.MethodPost, "/hermes/integrations/github/grant", url.Values{"allowed": {"on"}}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK || view.Granted != 1 || !view.Rows[0].Hermes {
		t.Fatalf("grant status = %d, granted = %d", rec.Code, view.Granted)
	}

	// Unlink clears the value.
	rec = serve(f.handler, formRequest(http.MethodPost, "/hermes/integrations/github/unlink", url.Values{}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusOK || view.Rows[0].StatusKey != "missing" {
		t.Fatalf("unlink status = %d, row = %+v", rec.Code, view.Rows[0])
	}

	// A missing required field is a 400 and writes nothing.
	rec = serve(f.handler, formRequest(http.MethodPost, "/hermes/integrations/spotify", url.Values{}))
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing field status = %d", rec.Code)
	}
}

func TestRoutesPageExplainsDeterministicSurface(t *testing.T) {
	f := newPhraseFixture(t, nil)
	var view struct {
		HasRegistry bool
		Totals      struct{ Tools, FastPath, AgentOnly, LocalOnly, Confirm, Hermes int }
		Rows        []struct {
			Name, Description, Permission                            string
			FastPath, AgentOnly, LocalOnly, Confirm, Enabled, Hermes bool
			ControlPhrases, Examples                                 int
		}
		Prefixes    []string
		Filler      string
		Unavailable string
	}
	rec := getJSON(t, f.handler, "/api/ui/routes", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if view.HasRegistry {
		t.Fatal("HasRegistry = true with no registry")
	}
	if view.Filler == "" {
		t.Fatal("filler missing from the routes view")
	}
}

// formRequest builds an authenticated same-origin form POST, the shape the
// React app sends.
func formRequest(method, target string, values url.Values) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	r.Host = "127.0.0.1:8082"
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Tailscale-User-Login", owner)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func TestPhrasebookGeneratorHiddenWithoutAKey(t *testing.T) {
	handler := newHandler(t, dashboard.Options{
		PhraseGen:      fakePhraseGen{reply: `{}`},
		PhraseGenReady: func() bool { return false },
	})
	var view phraseResp
	getJSON(t, handler, "/api/ui/phrasebook", &view)
	if view.HasGenerator {
		t.Fatal("HasGenerator = true without a provider key")
	}
}

type routeIn struct{}

type routeOut struct{ Text string }

func routeTool(name string, perm tool.Permission, agentOnly, speak bool) tool.Tool[routeIn, routeOut] {
	t := tool.Tool[routeIn, routeOut]{
		Name:        name,
		Description: name,
		Permission:  perm,
		LocalOnly:   tool.LocalOnlyNo,
		AgentOnly:   agentOnly,
		Handler: func(context.Context, routeIn) (routeOut, error) {
			return routeOut{Text: "ok"}, nil
		},
	}
	if speak {
		t.Speak = func(routeOut) string { return "ok" }
	}
	if !agentOnly {
		t.Examples = []string{"do " + name}
	}
	return t
}

func TestRoutesPageClassifiesLiveRegistry(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	for _, item := range []tool.Tool[routeIn, routeOut]{
		routeTool("test.act", tool.PermissionAct, false, true),
		routeTool("test.confirm", tool.PermissionConfirm, false, true),
		routeTool("test.agentonly", tool.PermissionRead, true, true),
		routeTool("test.read", tool.PermissionRead, false, false),
	} {
		if err := tool.Add(reg, item); err != nil {
			t.Fatalf("register %s: %v", item.Name, err)
		}
	}
	handler := newHandler(t, dashboard.Options{Registry: reg})
	var view struct {
		HasRegistry bool
		Totals      struct{ Tools, FastPath, AgentOnly, LocalOnly, Confirm, Hermes int }
		Rows        []struct {
			Name     string
			FastPath bool
			Confirm  bool
			Hermes   bool
		}
	}
	getJSON(t, handler, "/api/ui/routes", &view)
	if !view.HasRegistry || view.Totals.Tools != 4 {
		t.Fatalf("view = %+v", view)
	}
	if view.Totals.FastPath != 1 {
		t.Fatalf("FastPath = %d, want 1 (only test.act)", view.Totals.FastPath)
	}
	if view.Totals.Hermes != 3 {
		t.Fatalf("Hermes = %d, want 3 (all non-local read/act Tools; confirm is withheld)", view.Totals.Hermes)
	}
	want := map[string]struct{ fast, confirm bool }{
		"test.act":       {true, false},
		"test.confirm":   {false, true},
		"test.agentonly": {false, false},
		"test.read":      {false, false},
	}
	for _, row := range view.Rows {
		expected, ok := want[row.Name]
		if !ok {
			t.Fatalf("unexpected row %s", row.Name)
		}
		if row.FastPath != expected.fast || row.Confirm != expected.confirm {
			t.Errorf("%s = fast:%v confirm:%v, want %+v", row.Name, row.FastPath, row.Confirm, expected)
		}
	}
}

func TestPhrasebookPersonaPathMustExist(t *testing.T) {
	f := newPhraseFixture(t, fakePhraseGen{})
	for _, target := range []string{"/phrasebook/NoSuchPersona", "/phrasebook/NoSuchPersona/reset"} {
		rec := serve(f.handler, formRequest(http.MethodPost, target, url.Values{"tool": {"timer.set"}, "phrase": {"Set."}}))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, rec.Code)
		}
	}
}

func TestPhrasebookQueryShowsPersonaOverrides(t *testing.T) {
	f := newPhraseFixture(t, fakePhraseGen{})
	rec := serve(f.handler, formRequest(http.MethodPost, "/phrasebook/Scuzz", url.Values{"tool": {"timer.set"}, "phrase": {"Timer's set, fucker."}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d", rec.Code)
	}
	var view phraseResp
	getJSON(t, f.handler, "/api/ui/phrasebook?persona=Scuzz", &view)
	if view.Selected != "Scuzz" {
		t.Fatalf("selected = %q", view.Selected)
	}
	if view.Overridden != 1 {
		t.Fatalf("overridden = %d, want 1", view.Overridden)
	}
	for _, slot := range view.Slots {
		if slot.Tool == "timer.set" {
			if slot.Phrase != "Timer's set, fucker." || slot.Source != "manual" || !slot.Overridden {
				t.Fatalf("timer.set slot = %+v", slot)
			}
		}
	}
}

func TestHermesGrantRequiresAnAgentSurface(t *testing.T) {
	f := newPhraseFixture(t, nil)
	rec := serve(f.handler, formRequest(http.MethodPost, "/hermes/integrations/monster/grant", url.Values{"allowed": {"on"}}))
	var view hermesResp
	decodeJSON(t, rec, &view)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("monster grant status = %d, want 400", rec.Code)
	}
	if view.Granted != 0 {
		t.Fatalf("granted = %d, want 0", view.Granted)
	}
	rec = serve(f.handler, formRequest(http.MethodPost, "/hermes/integrations/nope/unlink", url.Values{}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown unlink status = %d, want 404", rec.Code)
	}
}
