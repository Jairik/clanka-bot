package wakesample

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxCustomPhrases bounds how many phrases the owner can add, so a stuck
// button or a script cannot fill the samples volume with directories.
const MaxCustomPhrases = 50

var (
	// ErrInvalidPhrase is a name SlugFor cannot turn into a path-safe slug.
	ErrInvalidPhrase = errors.New("use one to four words of letters and digits, at most 40 characters")
	// ErrPhraseExists is a phrase that is already listed, built-in or stored.
	ErrPhraseExists = errors.New("that phrase already exists")
	// ErrBuiltinPhrase is a built-in phrase, which is always listed.
	ErrBuiltinPhrase = errors.New("a built-in phrase cannot be removed")
	// ErrPhraseNotFound is a phrase with no directory.
	ErrPhraseNotFound = errors.New("that phrase is not stored")
	// ErrPhraseHasTakes is a phrase that still holds takes.
	ErrPhraseHasTakes = errors.New("that phrase still has takes")
)

// IsBuiltin reports one of BuiltinPhrases by slug.
func IsBuiltin(slug string) bool {
	for _, phrase := range BuiltinPhrases {
		if builtin, ok := SlugFor(phrase); ok && builtin == slug {
			return true
		}
	}
	return false
}

// ModelName is the openWakeWord model name the desktop trainer gives a slug
// (`phrase_key` in wakeword/training/train_cli.py): dashes become underscores.
func ModelName(slug string) string { return strings.ReplaceAll(slug, "-", "_") }

// isExportDir reports a phrase directory that is really the bundle directory,
// which New nests under the samples dir when no ExportDir is configured.
func (s *Service) isExportDir(slug string) bool {
	return filepath.Clean(filepath.Join(s.dir, slug)) == filepath.Clean(s.exportDir)
}

// AddPhrase stores a new phrase with no takes and returns its slug. The phrase
// is its directory, the same `<slug>/real/{positive,negative}` tree the
// recorder writes into, so it survives a reload and a restart without a second
// registry, and phrases recorded before this existed need no migration.
func (s *Service) AddPhrase(phrase string) (string, error) {
	slug, ok := SlugFor(phrase)
	if !ok {
		return "", ErrInvalidPhrase
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isExportDir(slug) {
		return "", fmt.Errorf("%q is reserved for bundles; choose another phrase", slug)
	}
	listed := s.Phrases()
	for _, existing := range listed {
		if existing == slug {
			return slug, fmt.Errorf("%w: %q", ErrPhraseExists, PhraseText(slug))
		}
	}
	if custom := len(listed) - len(BuiltinPhrases); custom >= MaxCustomPhrases {
		return "", fmt.Errorf("there are already %d custom phrases; remove an empty one first", custom)
	}
	for _, kind := range []Kind{Positive, Negative} {
		dir, err := s.kindDir(slug, kind)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	return slug, nil
}

// RemovePhrase deletes a custom phrase that has no takes. It removes only the
// known directories, each with os.Remove, so any file left in the tree (a take,
// a stray or unparsable WAV) makes it refuse instead of deleting anything.
func (s *Service) RemovePhrase(slug string) error {
	if !ValidSlug(slug) {
		return ErrInvalidPhrase
	}
	if IsBuiltin(slug) {
		return ErrBuiltinPhrase
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isExportDir(slug) {
		return ErrPhraseNotFound
	}
	root := filepath.Join(s.dir, slug)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return ErrPhraseNotFound
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrPhraseNotFound
	}
	if takes := s.Count(slug, Positive) + s.Count(slug, Negative); takes > 0 {
		return fmt.Errorf("%w: delete its %d take(s) first", ErrPhraseHasTakes, takes)
	}
	for _, dir := range []string{
		filepath.Join(root, "real", Positive.dir()),
		filepath.Join(root, "real", Negative.dir()),
		filepath.Join(root, "real"),
		root,
	} {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s holds other files, so nothing was removed", ErrPhraseHasTakes, PhraseText(slug))
		}
	}
	return nil
}

// ModelState is what the Assistant knows about a phrase's trained detector.
type ModelState int

const (
	// ModelUnknown means the wakeword service did not answer, or is not
	// configured, so the Assistant cannot tell.
	ModelUnknown ModelState = iota
	// ModelMissing means the service answered and does not advertise it.
	ModelMissing
	// ModelTrained means the service advertises the model by name.
	ModelTrained
)

// PhraseStatus is one phrase's readiness: takes kept and whether a trained
// model is served.
type PhraseStatus struct {
	Slug      string
	Builtin   bool
	Positives int
	Negatives int
	Model     ModelState
}

// Missing lists what the phrase still lacks, in the order the owner would fix
// it. An unknown model is not listed: the caller reports an unreachable
// wakeword service once rather than flagging every phrase.
func (p PhraseStatus) Missing() []string {
	var missing []string
	switch {
	case p.Positives == 0:
		missing = append(missing, "no positive samples")
	case p.Positives < MinTakes:
		missing = append(missing, fmt.Sprintf("only %d of %d positive samples", p.Positives, MinTakes))
	}
	if p.Negatives == 0 {
		missing = append(missing, "no negative samples")
	}
	if p.Model == ModelMissing {
		missing = append(missing, "no trained model")
	}
	return missing
}

// Statuses reports every listed phrase. trained is the set of model names the
// wakeword service advertises; known is false when it could not be asked.
func (s *Service) Statuses(trained map[string]bool, known bool) []PhraseStatus {
	phrases := s.Phrases()
	statuses := make([]PhraseStatus, 0, len(phrases))
	for _, slug := range phrases {
		status := PhraseStatus{
			Slug:      slug,
			Builtin:   IsBuiltin(slug),
			Positives: s.countNames(slug, Positive),
			Negatives: s.countNames(slug, Negative),
			Model:     ModelUnknown,
		}
		if known {
			status.Model = ModelMissing
			if trained[ModelName(slug)] {
				status.Model = ModelTrained
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// countNames counts take files by name alone. The status marks are refreshed
// after every kept take, so they list directories instead of opening every
// WAV header the way Count does; the selected phrase still uses Count.
func (s *Service) countNames(slug string, kind Kind) int {
	dir, err := s.kindDir(slug, kind)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".wav")
		if entry.Type().IsRegular() && id != entry.Name() && takePattern.MatchString(id) && strings.HasPrefix(id, kind.prefix()) {
			count++
		}
	}
	return count
}
