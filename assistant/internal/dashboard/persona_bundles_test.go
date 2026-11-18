package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/persona"
	"github.com/Jairik/clanka-bot/assistant/internal/phrasebook"
)

func TestPersonaBundleRoundTripKeepsOriginal(t *testing.T) {
	f := newPhraseFixture(t, nil)
	ctx := context.Background()
	if _, err := f.personas.Select(ctx, "Scuzz"); err != nil {
		t.Fatal(err)
	}
	// Include an override for a tool missing from this Dashboard's slot list.
	if _, err := f.phrases.Set(ctx, "Scuzz", "spotify.pause", `'"Paused."'`, phrasebook.SourceLLM); err != nil {
		t.Fatal(err)
	}
	if _, err := f.phrases.Set(ctx, "Scuzz", "timer.set", "Timer on.", phrasebook.SourceManual); err != nil {
		t.Fatal(err)
	}
	exportURL := "/api/ui/persona-bundles/export?name=Scuzz"
	if rec := serve(f.handler, request(http.MethodGet, exportURL, "")); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated export returned %d", rec.Code)
	}
	var bundle struct {
		Version int
		Persona persona.Input
		Phrases []phrasebook.PortablePhrase
	}
	exported := getJSON(t, f.handler, exportURL, &bundle)
	if exported.Code != http.StatusOK || !strings.Contains(exported.Header().Get("Content-Disposition"), "attachment") ||
		bundle.Version != 1 || len(bundle.Phrases) != 2 {
		t.Fatalf("invalid download: %d %s", exported.Code, exported.Body.String())
	}

	importURL := "/api/ui/persona-bundles/import"
	body := url.Values{"bundle": {exported.Body.String()}, "name": {"scuzz"}}
	if rec := serve(f.handler, formRequest(http.MethodPost, importURL, body)); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate import returned %d", rec.Code)
	}
	body.Set("name", "Scuzz imported")
	req := formRequest(http.MethodPost, importURL, body)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://other.example")
	if rec := serve(f.handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin import returned %d", rec.Code)
	}
	if rec := serve(f.handler, formRequest(http.MethodPost, importURL, body)); rec.Code != http.StatusCreated {
		t.Fatalf("import returned %d: %s", rec.Code, rec.Body.String())
	}
	copyURL := "/api/ui/persona-bundles/copy"
	form := url.Values{"source": {"Scuzz"}, "name": {"Scuzz saved"}}
	if rec := serve(f.handler, formRequest(http.MethodPost, copyURL, form)); rec.Code != http.StatusCreated {
		t.Fatalf("save copy returned %d: %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"Scuzz", "Scuzz imported", "Scuzz saved"} {
		want := bundle.Persona
		want.Name = name
		p, ok := f.personas.Get(name)
		if !ok || !reflect.DeepEqual(p.Input(), want) {
			t.Errorf("persona %q differs from export", name)
		}
		got := f.phrases.Entries(name)
		if len(got) != len(bundle.Phrases) {
			t.Fatalf("%q has %d phrase overrides", name, len(got))
		}
		for i, phrase := range bundle.Phrases {
			if got[i].Tool != phrase.Tool || got[i].Phrase != phrase.Phrase || got[i].Source != phrase.Source {
				t.Errorf("%q override differs from export: %+v", name, got[i])
			}
		}
	}
	if active := f.settings.String(persona.ActiveKey); active != "Scuzz" {
		t.Fatalf("active persona changed to %q", active)
	}
}

func TestPersonaBundleRejectsInvalidPhrasesBeforeWriting(t *testing.T) {
	f := newPhraseFixture(t, nil)
	original := f.personas.List()
	bundle := map[string]any{
		"version": 1,
		"persona": persona.Input{Name: "Portable", Voice: "Another Pi voice", Personality: "Kind", Filler: "One sec.", Unavailable: "Try again."},
		"phrases": []phrasebook.PortablePhrase{
			{Tool: "timer.set", Phrase: "Set.", Source: phrasebook.SourceManual},
			{Tool: "timer.set", Phrase: "Duplicate.", Source: phrasebook.SourceManual},
		},
	}
	post := func(t *testing.T, want int) {
		t.Helper()
		raw, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		rec := serve(f.handler, formRequest(http.MethodPost, "/api/ui/persona-bundles/import", url.Values{"bundle": {string(raw)}}))
		if rec.Code != want {
			t.Fatalf("import returned %d, want %d: %s", rec.Code, want, rec.Body.String())
		}
	}
	post(t, http.StatusBadRequest)
	if !reflect.DeepEqual(f.personas.List(), original) || len(f.phrases.Entries("Portable")) > 0 {
		t.Fatal("rejected import changed the library")
	}
	// A voice reference can be saved before that voice is available locally.
	bundle["phrases"] = []phrasebook.PortablePhrase{}
	post(t, http.StatusCreated)
	if _, err := f.personas.Select(context.Background(), "Portable"); err == nil {
		t.Fatal("selected a persona with an unavailable voice")
	}
}
