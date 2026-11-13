package dashboard_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/wakesample"
)

// phraseStatusView is the part of the voice-samples view these tests read.
type phraseStatusView struct {
	Flash, Error, Slug, Phrase string
	AddPhraseAction            string
	ModelsKnown                bool
	ModelNote                  string
	Phrases                    []struct {
		Slug, Text, Model, StatusLabel, RemoveAction string
		Selected, Builtin, Stored, NeedsAttention    bool
		CanRemove                                    bool
		Positives, Negatives                         int
		Missing                                      []string
	}
}

func (v phraseStatusView) phrase(t *testing.T, slug string) (found struct {
	Slug, Text, Model, StatusLabel, RemoveAction string
	Selected, Builtin, Stored, NeedsAttention    bool
	CanRemove                                    bool
	Positives, Negatives                         int
	Missing                                      []string
}) {
	t.Helper()
	for _, p := range v.Phrases {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("phrase %q not listed in %+v", slug, v.Phrases)
	return found
}

func decodePhrases(t *testing.T, body []byte) phraseStatusView {
	t.Helper()
	var view phraseStatusView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return view
}

func form(values url.Values) []byte { return []byte(values.Encode()) }

const formType = "application/x-www-form-urlencoded"

func TestAddPhrasePersistsWithoutTakesAndSurvivesARestart(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	var page phraseStatusView
	getJSON(t, f.handler, "/api/ui/voice-samples", &page)
	if page.AddPhraseAction != "/voice-samples/phrases" {
		t.Fatalf("AddPhraseAction = %q", page.AddPhraseAction)
	}

	rec := f.post(page.AddPhraseAction, form(url.Values{"phrase": {"Hey Clanka"}, "current": {"clanker"}}), formType)
	if rec.Code != http.StatusOK {
		t.Fatalf("add status = %d: %s", rec.Code, rec.Body.String())
	}
	view := decodePhrases(t, rec.Body.Bytes())
	if view.Slug != "hey-clanka" || !strings.Contains(view.Flash, "hey-clanka") {
		t.Fatalf("slug/flash = %q / %q; the owner must see the normalised name", view.Slug, view.Flash)
	}
	if p := view.phrase(t, "hey-clanka"); !p.Selected || !p.Stored || p.Builtin {
		t.Errorf("added phrase = %+v", p)
	}

	// A fresh store and handler over the same directory: the phrase is on disk,
	// not in the URL.
	again, err := wakesample.New(wakesample.Options{Dir: f.store.Dir(), ExportDir: f.store.ExportDir()})
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandler(t, dashboard.Options{VoiceSamples: again})
	var reloaded phraseStatusView
	getJSON(t, handler, "/api/ui/voice-samples", &reloaded)
	if p := reloaded.phrase(t, "hey-clanka"); !p.Stored || p.Positives != 0 {
		t.Errorf("after restart = %+v", p)
	}
}

func TestAddPhraseRejectsDuplicatesAndInvalidNames(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	for _, tc := range []struct {
		phrase string
		status int
		want   string
	}{
		{"Volume Up", http.StatusConflict, "already exists"},
		{"../etc/passwd", http.StatusBadRequest, "not a valid phrase"},
		{"   ", http.StatusBadRequest, "not a valid phrase"},
		{"one two three four five", http.StatusBadRequest, "not a valid phrase"},
	} {
		rec := f.post("/voice-samples/phrases", form(url.Values{"phrase": {tc.phrase}, "current": {"volume-down"}}), formType)
		if rec.Code != tc.status {
			t.Errorf("%q: status = %d, want %d: %s", tc.phrase, rec.Code, tc.status, rec.Body.String())
			continue
		}
		view := decodePhrases(t, rec.Body.Bytes())
		if !strings.Contains(view.Error, tc.want) {
			t.Errorf("%q: error = %q, want it to mention %q", tc.phrase, view.Error, tc.want)
		}
		if view.Slug != "volume-down" {
			t.Errorf("%q: a refused add moved the page to %q", tc.phrase, view.Slug)
		}
	}
	if entries, _ := os.ReadDir(f.store.Dir()); len(entries) != 0 {
		t.Errorf("a refused add created %d entries", len(entries))
	}
}

func TestAddPhraseNeedsTheMutatingGuard(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	rec := serve(f.handler, request(http.MethodPost, "/voice-samples/phrases?phrase=x", owner))
	if rec.Code == http.StatusOK {
		t.Fatalf("a POST without same-origin headers was accepted")
	}
	if entries, _ := os.ReadDir(f.store.Dir()); len(entries) != 0 {
		t.Fatalf("unguarded POST created a phrase")
	}
}

func TestPhraseStatusUsesTheAdvertisedModels(t *testing.T) {
	dir := t.TempDir()
	store, err := wakesample.New(wakesample.Options{Dir: filepath.Join(dir, "s"), ExportDir: filepath.Join(dir, "e")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < wakesample.MinTakes; i++ {
		if _, err := store.Add("clanker", wakesample.Positive, take(1, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Add("clanker", wakesample.Negative, take(1, 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("volume-up", wakesample.Positive, take(1, 50)); err != nil {
		t.Fatal(err)
	}
	addr := fakeWakewordServer(t, "clanker", "volume_up", "okay_nabu")
	handler := newHandler(t, dashboard.Options{VoiceSamples: store, Wakeword: dashboard.WakewordInfo{Addr: addr}})
	var view phraseStatusView
	getJSON(t, handler, "/api/ui/voice-samples", &view)
	if !view.ModelsKnown {
		t.Fatalf("ModelsKnown = false: %q", view.ModelNote)
	}

	clanker := view.phrase(t, "clanker")
	if clanker.NeedsAttention || clanker.Model != "trained" || len(clanker.Missing) != 0 {
		t.Errorf("complete clanker = %+v", clanker)
	}
	up := view.phrase(t, "volume-up")
	if !up.NeedsAttention || up.Model != "trained" || !reflectEqual(up.Missing, []string{"only 1 of 5 positive samples", "no negative samples"}) {
		t.Errorf("volume-up = %+v", up)
	}
	down := view.phrase(t, "volume-down")
	if !down.NeedsAttention || down.Model != "missing" || !reflectEqual(down.Missing, []string{"no positive samples", "no negative samples", "no trained model"}) {
		t.Errorf("volume-down = %+v", down)
	}
	if !strings.Contains(down.StatusLabel, "no trained model") {
		t.Errorf("status label = %q", down.StatusLabel)
	}
}

func TestPhraseStatusSaysWhenModelsAreUnknown(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	var view phraseStatusView
	getJSON(t, f.handler, "/api/ui/voice-samples", &view)
	if view.ModelsKnown || view.ModelNote == "" {
		t.Fatalf("known/note = %v/%q with no wakeword service", view.ModelsKnown, view.ModelNote)
	}
	// Still flagged: a phrase with no samples needs attention either way.
	if p := view.phrase(t, "clanker"); !p.NeedsAttention || p.Model != "unknown" {
		t.Errorf("clanker = %+v", p)
	}
}

func TestRemovePhraseRefusesTakesAndBuiltins(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	if rec := f.post("/voice-samples/phrases", form(url.Values{"phrase": {"good night"}}), formType); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	var view phraseStatusView
	getJSON(t, f.handler, "/api/ui/voice-samples?phrase=good-night", &view)
	p := view.phrase(t, "good-night")
	if !p.CanRemove || p.RemoveAction != "/voice-samples/phrases/good-night/remove" {
		t.Fatalf("empty custom phrase = %+v", p)
	}
	if c := view.phrase(t, "clanker"); c.CanRemove || c.RemoveAction != "" {
		t.Errorf("builtin offers removal: %+v", c)
	}
	if rec := f.post("/voice-samples/phrases/clanker/remove", nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("remove builtin = %d", rec.Code)
	}

	upload := f.post("/voice-samples/takes/good-night/positive", take(1, 9), "audio/wav")
	if upload.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", upload.Code, upload.Body.String())
	}
	if after := decodePhrases(t, upload.Body.Bytes()).phrase(t, "good-night"); after.CanRemove {
		t.Errorf("phrase with a take offers removal: %+v", after)
	}
	rec := f.post(p.RemoveAction, nil, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("remove with takes = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodePhrases(t, rec.Body.Bytes()); !strings.Contains(got.Error, "take") || got.Slug != "good-night" {
		t.Errorf("refusal = %q on %q", got.Error, got.Slug)
	}
	if f.store.Count("good-night", wakesample.Positive) != 1 {
		t.Fatal("a refused remove deleted the take")
	}

	if rec := f.post("/voice-samples/clear/good-night/positive", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d", rec.Code)
	}
	rec = f.post(p.RemoveAction, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove empty = %d: %s", rec.Code, rec.Body.String())
	}
	done := decodePhrases(t, rec.Body.Bytes())
	if done.Slug != "clanker" || !strings.Contains(done.Flash, "good night") {
		t.Errorf("after remove: slug %q flash %q", done.Slug, done.Flash)
	}
	for _, phrase := range done.Phrases {
		if phrase.Slug == "good-night" {
			t.Fatal("removed phrase still listed")
		}
	}
}

func TestAnUnsavedURLPhraseIsShownButNotStored(t *testing.T) {
	f := newVoiceSamplesFixture(t, wakesample.Options{})
	var view phraseStatusView
	getJSON(t, f.handler, "/api/ui/voice-samples?phrase=good-morning", &view)
	p := view.phrase(t, "good-morning")
	if p.Stored || !p.Selected || !p.NeedsAttention || p.CanRemove {
		t.Errorf("url-only phrase = %+v", p)
	}
}

func reflectEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
