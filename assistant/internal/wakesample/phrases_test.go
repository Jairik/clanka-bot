package wakesample

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAddPhrasePersistsAcrossAFreshService(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: filepath.Join(dir, "samples"), ExportDir: filepath.Join(dir, "exports")}
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	slug, err := first.AddPhrase("  Hey Clanka ")
	if err != nil || slug != "hey-clanka" {
		t.Fatalf("AddPhrase = %q, %v", slug, err)
	}
	// The phrase is the same on-disk shape the recorder writes, so a take
	// added later lands beside it and the desktop import reads it unchanged.
	for _, kind := range []string{"positive", "negative"} {
		if info, err := os.Stat(filepath.Join(opts.Dir, "hey-clanka", "real", kind)); err != nil || !info.IsDir() {
			t.Fatalf("kind dir %s: %v", kind, err)
		}
	}
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(second.Phrases(), "hey-clanka") {
		t.Fatalf("a fresh Service lost the phrase: %v", second.Phrases())
	}
	if second.Count("hey-clanka", Positive) != 0 {
		t.Fatal("an added phrase must start with no takes")
	}
	keep(t, second, "hey-clanka", Positive, 1)
	if second.Count("hey-clanka", Positive) != 1 {
		t.Fatal("a take into an added phrase was not kept")
	}
}

func TestAddPhraseRejectsInvalidAndDuplicateNames(t *testing.T) {
	service := newStore(t, Options{})
	for _, bad := range []string{"", "   ", "../etc", "a/b", "123", "one two three four five", strings.Repeat("a", 41), "!!!"} {
		if _, err := service.AddPhrase(bad); !errors.Is(err, ErrInvalidPhrase) {
			t.Errorf("AddPhrase(%q) = %v, want ErrInvalidPhrase", bad, err)
		}
	}
	for _, dup := range []string{"clanker", "Volume Up", "volume-down"} {
		if _, err := service.AddPhrase(dup); !errors.Is(err, ErrPhraseExists) {
			t.Errorf("AddPhrase(%q) = %v, want ErrPhraseExists", dup, err)
		}
	}
	if _, err := service.AddPhrase("hey clanka"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AddPhrase("HEY-CLANKA"); !errors.Is(err, ErrPhraseExists) {
		t.Errorf("second add = %v, want ErrPhraseExists", err)
	}
	// A phrase that already has takes from the old flow is a duplicate too.
	keep(t, service, "good-night", Positive, 1)
	if _, err := service.AddPhrase("good night"); !errors.Is(err, ErrPhraseExists) {
		t.Errorf("add over stored takes = %v, want ErrPhraseExists", err)
	}
}

func TestAddPhraseIsBounded(t *testing.T) {
	service := newStore(t, Options{})
	for i := 0; i < MaxCustomPhrases; i++ {
		if _, err := service.AddPhrase("phrase " + string(rune('a'+i%26)) + " " + string(rune('a'+i/26))); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := service.AddPhrase("one more"); err == nil || errors.Is(err, ErrInvalidPhrase) {
		t.Fatalf("over-cap add = %v, want a limit error", err)
	}
}

func TestPhrasesSkipsANestedExportDir(t *testing.T) {
	dir := t.TempDir()
	service, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if contains(service.Phrases(), "exports") {
		t.Fatalf("the default export dir is listed as a phrase: %v", service.Phrases())
	}
	if _, err := service.AddPhrase("exports"); err == nil {
		t.Fatal("added a phrase on top of the export dir")
	}
}

func TestRemovePhraseOnlyRemovesAnEmptyCustomPhrase(t *testing.T) {
	service := newStore(t, Options{})
	if err := service.RemovePhrase("clanker"); !errors.Is(err, ErrBuiltinPhrase) {
		t.Errorf("remove builtin = %v", err)
	}
	if err := service.RemovePhrase("../x"); !errors.Is(err, ErrInvalidPhrase) {
		t.Errorf("remove invalid = %v", err)
	}
	if err := service.RemovePhrase("never-added"); !errors.Is(err, ErrPhraseNotFound) {
		t.Errorf("remove missing = %v", err)
	}

	if _, err := service.AddPhrase("hey clanka"); err != nil {
		t.Fatal(err)
	}
	take := keep(t, service, "hey-clanka", Negative, 1)
	if err := service.RemovePhrase("hey-clanka"); !errors.Is(err, ErrPhraseHasTakes) {
		t.Fatalf("remove with takes = %v, want ErrPhraseHasTakes", err)
	}
	if service.Count("hey-clanka", Negative) != 1 {
		t.Fatal("a refused remove deleted a take")
	}
	if err := service.Delete("hey-clanka", Negative, take.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.RemovePhrase("hey-clanka"); err != nil {
		t.Fatalf("remove empty = %v", err)
	}
	if contains(service.Phrases(), "hey-clanka") {
		t.Fatal("removed phrase is still listed")
	}
	if _, err := os.Stat(filepath.Join(service.Dir(), "hey-clanka")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("phrase dir left behind: %v", err)
	}
}

func TestRemovePhraseNeverDeletesAStrayFile(t *testing.T) {
	service := newStore(t, Options{})
	if _, err := service.AddPhrase("hey clanka"); err != nil {
		t.Fatal(err)
	}
	// An unparsable WAV is not counted as a take, but it is still the owner's
	// file: removal must refuse rather than delete it.
	stray := filepath.Join(service.Dir(), "hey-clanka", "real", "positive", "real_p_00001.wav")
	if err := os.WriteFile(stray, []byte("not a wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.RemovePhrase("hey-clanka"); err == nil {
		t.Fatal("removed a phrase that still holds a file")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("stray file was deleted: %v", err)
	}
}

func TestModelNameMatchesTheTrainer(t *testing.T) {
	for slug, want := range map[string]string{"clanker": "clanker", "volume-up": "volume_up", "hey-clanka-bot": "hey_clanka_bot"} {
		if got := ModelName(slug); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestMissingListsEveryGap(t *testing.T) {
	cases := []struct {
		name   string
		status PhraseStatus
		want   []string
	}{
		{"brand new", PhraseStatus{Model: ModelMissing}, []string{"no positive samples", "no negative samples", "no trained model"}},
		{"too few", PhraseStatus{Positives: 2, Negatives: 3, Model: ModelTrained}, []string{"only 2 of 5 positive samples"}},
		{"no negatives", PhraseStatus{Positives: MinTakes, Model: ModelTrained}, []string{"no negative samples"}},
		{"untrained", PhraseStatus{Positives: 20, Negatives: 4, Model: ModelMissing}, []string{"no trained model"}},
		{"complete", PhraseStatus{Positives: 20, Negatives: 4, Model: ModelTrained}, nil},
		// An unknown model is not reported as missing: the service did not
		// answer, so the page says so once instead of flagging every phrase.
		{"unknown model", PhraseStatus{Positives: 20, Negatives: 4, Model: ModelUnknown}, nil},
		{"unknown and empty", PhraseStatus{Model: ModelUnknown}, []string{"no positive samples", "no negative samples"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.Missing(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Missing() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusesCountTakesAndResolveModels(t *testing.T) {
	service := newStore(t, Options{})
	if _, err := service.AddPhrase("hey clanka"); err != nil {
		t.Fatal(err)
	}
	keep(t, service, "hey-clanka", Positive, 1)
	keep(t, service, "volume-up", Negative, 1)
	trained := map[string]bool{"clanker": true, "volume_up": true}
	byslug := map[string]PhraseStatus{}
	for _, status := range service.Statuses(trained, true) {
		byslug[status.Slug] = status
	}
	if s := byslug["clanker"]; !s.Builtin || s.Model != ModelTrained || s.Positives != 0 {
		t.Errorf("clanker = %+v", s)
	}
	if s := byslug["volume-up"]; s.Model != ModelTrained || s.Negatives != 1 {
		t.Errorf("volume-up = %+v", s)
	}
	if s := byslug["volume-down"]; s.Model != ModelMissing {
		t.Errorf("volume-down = %+v", s)
	}
	if s := byslug["hey-clanka"]; s.Builtin || s.Positives != 1 || s.Model != ModelMissing {
		t.Errorf("hey-clanka = %+v", s)
	}
	for _, status := range service.Statuses(nil, false) {
		if status.Model != ModelUnknown {
			t.Errorf("%s model = %v with no answer from the service", status.Slug, status.Model)
		}
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
