// Package persona owns the persistent Persona library and the atomic switch of
// one bundle: voice, personality prompt, Agent Filler and Unavailable wording.
// The personality prompt is read only by the Agent path's single prompt builder
// (ADR-0002); selecting a Persona writes the same live settings the voice and
// wording Tools use, in one transaction.
package persona

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

// Settings keys and validation bounds.
const (
	// ActiveKey names the selected Persona. An empty value means none; the
	// voice and wording settings stay whatever they were last set to.
	ActiveKey = "persona.active"
	// MaxNameRunes bounds a spoken Persona name.
	MaxNameRunes = 64
	// MaxPersonalityRunes bounds the personality prompt. It is sent inside the
	// one labelled system part, never as history or a Tool result.
	MaxPersonalityRunes = 8000
	// MaxWordingRunes bounds the Filler and Unavailable wording, which are
	// spoken and synthesized like any other wording setting.
	MaxWordingRunes = 500
)

// Sentinels let callers map a rejection to a spoken or HTTP response without
// inspecting message text.
var (
	ErrUnknownPersona   = errors.New("persona: unknown persona")
	ErrPersonaExists    = errors.New("persona: persona already exists")
	ErrInvalidPersona   = errors.New("persona: invalid persona")
	ErrVoiceUnavailable = errors.New("persona: voice unavailable")
	ErrNoActivePersona  = errors.New("persona: no active persona")
	ErrNoSettings       = errors.New("persona: settings service is not available")
)

// Error carries a user-safe message beside a sentinel. The message is rendered
// escaped by the Dashboard and bounded by the Tool's own failure line.
type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Kind.Error() + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Kind }

// Message returns the user-safe text of a persona error, or "" for another
// error. Callers must not display the wrapped internal error itself.
func Message(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Message
	}
	return ""
}

// Persona is one stored bundle. Name is stable; editing changes the other
// fields but never renames the row.
type Persona struct {
	Name        string
	Voice       string
	Personality string
	Filler      string
	Unavailable string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Input is a create or edit form. It contains no times; the service owns them.
type Input struct {
	Name        string
	Voice       string
	Personality string
	Filler      string
	Unavailable string
}

// VoiceLibrary is the read-only voice seam the Persona service validates
// against. Production passes the one voicelib.Service; tests may use a fake.
type VoiceLibrary interface {
	Get(name string) (voicelib.Voice, bool)
	Active() (voicelib.Voice, bool)
	DefaultName() string
	SpeechRef(name string) (string, error)
}

// Current is the active Persona plus the settings that actually apply. The
// differs flags are comparisons, not proof of who changed the value: a manual
// voice/wording override, or an edit to the Persona after selection, both show
// up here and the caller must not claim the exact bundle in that case.
type Current struct {
	Persona               Persona
	EffectiveVoice        string
	EffectiveVoiceBuiltin bool
	EffectiveFiller       string
	EffectiveUnavailable  string
	VoiceDiffers          bool
	FillerDiffers         bool
	UnavailableDiffers    bool
	Exact                 bool
	PersonaVoiceReady     bool
}

// Personality is the per-turn prompt snapshot. It contains only the record's
// name and prompt; the prompt builder owns where it is sent.
type Personality struct {
	Name   string
	Prompt string
}

// Service is the in-process Persona library and selection seam.
type Service struct {
	db     *sql.DB
	voices VoiceLibrary
	logger *slog.Logger

	mu       sync.RWMutex
	personas map[string]Persona
	settings *settings.Service
}

// New opens the library from the one SQLite store. It does not need the
// settings service; Bind installs that after main creates the definitions.
func New(ctx context.Context, st *store.Store, voices VoiceLibrary, logger *slog.Logger) (*Service, error) {
	if st == nil || st.DB() == nil {
		return nil, errors.New("persona: store is required")
	}
	if voices == nil {
		return nil, errors.New("persona: voice library is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		db:       st.DB(),
		voices:   voices,
		logger:   logger,
		personas: make(map[string]Persona),
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Bind installs the one live settings service shared with the Dashboard and
// the voice/wording Tools. Selection writes through this service only.
func (s *Service) Bind(svc *settings.Service) {
	s.mu.Lock()
	s.settings = svc
	s.mu.Unlock()
}

// Definitions adds the active-Persona marker to the one settings service. It is
// a live string: selecting writes it in the same batch as the bundle, and a
// reset clears only the marker, leaving the applied voice and wording alone.
func (*Service) Definitions() []settings.Definition {
	return []settings.Definition{
		settings.String(ActiveKey, "",
			settings.WithDescription("Active Persona name. Selecting a Persona writes voice.name, agent.filler and agent.unavailable_message with it in one transaction; resetting this key leaves those applied values alone."),
			settings.WithValidator(validateActiveSetting),
		),
	}
}

// List returns every Persona in case-insensitive name order.
func (s *Service) List() []Persona {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Persona, 0, len(s.personas))
	for _, p := range s.personas {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Names returns the selectable names for the shared name matcher.
func (s *Service) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.personas))
	for _, p := range s.personas {
		out = append(out, p.Name)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// Get returns one Persona by name, case-insensitively.
func (s *Service) Get(name string) (Persona, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Persona{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.personas[name]; ok {
		return p, true
	}
	for key, p := range s.personas {
		if strings.EqualFold(key, name) {
			return p, true
		}
	}
	return Persona{}, false
}

// Create validates the bundle and inserts one Persona. A duplicate name,
// including a case-only duplicate, is refused before any row is written.
func (s *Service) Create(ctx context.Context, in Input) (Persona, error) {
	clean, err := s.validate(in)
	if err != nil {
		return Persona{}, err
	}
	if _, exists := s.Get(clean.Name); exists {
		return Persona{}, &Error{Kind: ErrPersonaExists, Message: fmt.Sprintf("A persona called %s already exists.", clean.Name)}
	}
	now := time.Now().UTC().Truncate(time.Second)
	p := Persona{
		Name:        clean.Name,
		Voice:       clean.Voice,
		Personality: clean.Personality,
		Filler:      clean.Filler,
		Unavailable: clean.Unavailable,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO personas (name, voice, personality, filler, unavailable, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Voice, p.Personality, p.Filler, p.Unavailable, stamp(p.CreatedAt), stamp(p.UpdatedAt)); err != nil {
		// The primary key collation is the last guard against a concurrent
		// create; the snapshot is only updated after a successful write.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Persona{}, &Error{Kind: ErrPersonaExists, Message: fmt.Sprintf("A persona called %s already exists.", clean.Name)}
		}
		return Persona{}, fmt.Errorf("persona: create %s: %w", clean.Name, err)
	}
	s.mu.Lock()
	s.personas[p.Name] = p
	s.mu.Unlock()
	s.logger.Info("persona created", "name", p.Name, "voice", p.Voice)
	return p, nil
}

// Update replaces the voice, personality, Filler and Unavailable wording of an
// existing Persona. The name is immutable. A Persona that is already active
// keeps its applied voice/wording until it is selected again; the personality
// prompt applies on the next turn because the prompt builder reads the record.
func (s *Service) Update(ctx context.Context, name string, in Input) (Persona, error) {
	existing, ok := s.Get(name)
	if !ok {
		return Persona{}, &Error{Kind: ErrUnknownPersona, Message: fmt.Sprintf("I couldn't find a persona called %s.", name)}
	}
	in.Name = existing.Name
	clean, err := s.validate(in)
	if err != nil {
		return Persona{}, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	updated := existing
	updated.Voice = clean.Voice
	updated.Personality = clean.Personality
	updated.Filler = clean.Filler
	updated.Unavailable = clean.Unavailable
	updated.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx,
		`UPDATE personas SET voice = ?, personality = ?, filler = ?, unavailable = ?, updated_at = ? WHERE name = ?`,
		updated.Voice, updated.Personality, updated.Filler, updated.Unavailable, stamp(updated.UpdatedAt), existing.Name); err != nil {
		return Persona{}, fmt.Errorf("persona: update %s: %w", existing.Name, err)
	}
	s.mu.Lock()
	s.personas[existing.Name] = updated
	s.mu.Unlock()
	s.logger.Info("persona updated", "name", updated.Name, "voice", updated.Voice)
	return updated, nil
}

// Select applies one Persona as one settings transaction: voice, Filler,
// Unavailable wording and the active marker. Every field and the voice state
// are validated before SetMany touches SQLite, so a stale or unknown voice
// cannot leave half of a Persona applied.
func (s *Service) Select(ctx context.Context, name string) (Persona, error) {
	p, ok := s.Get(name)
	if !ok {
		return Persona{}, &Error{Kind: ErrUnknownPersona, Message: fmt.Sprintf("I couldn't find a persona called %s.", name)}
	}
	svc := s.settingsService()
	if svc == nil {
		return Persona{}, &Error{Kind: ErrNoSettings, Message: "The settings service isn't available."}
	}
	if err := s.validateBundle(p); err != nil {
		return Persona{}, err
	}
	if err := svc.SetMany(ctx, map[string]any{
		voicelib.KeyName:       p.Voice,
		wording.FillerKey:      p.Filler,
		wording.UnavailableKey: p.Unavailable,
		ActiveKey:              p.Name,
	}); err != nil {
		s.logger.Error("persona select failed", "name", p.Name, "error", err.Error())
		return Persona{}, &Error{Kind: ErrInvalidPersona, Message: "The persona could not be applied; nothing was changed."}
	}
	s.logger.Info("persona selected", "name", p.Name, "voice", p.Voice)
	return p, nil
}

// ClearActive removes only the active marker. The applied voice and wording
// stay as they are; the Dashboard says so rather than pretending they revert.
func (s *Service) ClearActive(ctx context.Context) error {
	svc := s.settingsService()
	if svc == nil {
		return &Error{Kind: ErrNoSettings, Message: "The settings service isn't available."}
	}
	if _, err := svc.Reset(ctx, ActiveKey); err != nil {
		return err
	}
	return nil
}

// Current resolves the active Persona and compares its bundle with the live
// settings that will actually apply. It reports false when no Persona is
// selected or the stored name no longer exists.
func (s *Service) Current() (Current, bool) {
	svc := s.settingsService()
	if svc == nil {
		return Current{}, false
	}
	name := strings.TrimSpace(svc.String(ActiveKey))
	if name == "" {
		return Current{}, false
	}
	p, ok := s.Get(name)
	if !ok {
		return Current{}, false
	}
	current := Current{Persona: p}
	if active, ok := s.voices.Get(p.Voice); ok {
		current.PersonaVoiceReady = active.Ready()
	}
	if active, ok := s.voices.Active(); ok {
		if _, err := s.voices.SpeechRef(active.Name); err == nil {
			current.EffectiveVoice = active.Name
			current.EffectiveVoiceBuiltin = active.Builtin
			current.VoiceDiffers = !strings.EqualFold(active.Name, p.Voice)
		} else {
			// The persisted entry is unusable; the speaker falls back to the
			// default built-in, so report that rather than the stale name.
			current.EffectiveVoice = s.voices.DefaultName()
			current.VoiceDiffers = true
		}
	} else {
		current.VoiceDiffers = true
	}
	current.EffectiveFiller = svc.String(wording.FillerKey)
	current.FillerDiffers = !sameWording(current.EffectiveFiller, p.Filler)
	current.EffectiveUnavailable = svc.String(wording.UnavailableKey)
	current.UnavailableDiffers = !sameWording(current.EffectiveUnavailable, p.Unavailable)
	current.Exact = !current.VoiceDiffers && !current.FillerDiffers && !current.UnavailableDiffers
	return current, true
}

// Personality returns the active Persona's prompt for the Agent path. The
// caller reads it once per turn; there is no new egress path.
func (s *Service) Personality() Personality {
	current, ok := s.Current()
	if !ok {
		return Personality{}
	}
	return Personality{Name: current.Persona.Name, Prompt: current.Persona.Personality}
}

// ActiveVoiceRef resolves the active Persona's voice for startup warm-loading.
// A stale or unknown voice returns ErrVoiceUnavailable; the active voice has
// already been made safe by voicelib.EnsureActive, so this is never fatal.
func (s *Service) ActiveVoiceRef() (string, error) {
	current, ok := s.Current()
	if !ok {
		return "", ErrNoActivePersona
	}
	ref, err := s.voices.SpeechRef(current.Persona.Voice)
	if err != nil {
		return "", &Error{Kind: ErrVoiceUnavailable, Message: fmt.Sprintf("The %s persona's voice is not ready.", current.Persona.Name)}
	}
	return ref, nil
}

func (s *Service) settingsService() *settings.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// validate normalises and checks every field plus the voice state before a
// create or update writes anything.
func (s *Service) validate(in Input) (Input, error) {
	clean := Input{
		Name:        cleanName(in.Name),
		Voice:       strings.TrimSpace(in.Voice),
		Personality: strings.TrimSpace(in.Personality),
		Filler:      cleanWording(in.Filler),
		Unavailable: cleanWording(in.Unavailable),
	}
	if clean.Name == "" {
		return Input{}, invalid("Give the persona a name.")
	}
	if err := checkName(clean.Name); err != nil {
		return Input{}, err
	}
	if clean.Voice == "" {
		return Input{}, invalid("Choose a voice for the persona.")
	}
	if clean.Personality == "" {
		return Input{}, invalid("Write the personality prompt.")
	}
	if err := checkText(clean.Personality, MaxPersonalityRunes, true, "personality prompt"); err != nil {
		return Input{}, err
	}
	if clean.Filler == "" {
		return Input{}, invalid("Write the Filler phrase.")
	}
	if err := checkText(clean.Filler, MaxWordingRunes, false, "Filler phrase"); err != nil {
		return Input{}, err
	}
	if clean.Unavailable == "" {
		return Input{}, invalid("Write the Unavailable message.")
	}
	if err := checkText(clean.Unavailable, MaxWordingRunes, false, "Unavailable message"); err != nil {
		return Input{}, err
	}
	if err := s.validateBundle(Persona{
		Name:        clean.Name,
		Voice:       clean.Voice,
		Personality: clean.Personality,
		Filler:      clean.Filler,
		Unavailable: clean.Unavailable,
	}); err != nil {
		return Input{}, err
	}
	return clean, nil
}

// validateBundle checks every stored field and the voice before a select. A
// Persona whose voice went stale after creation, or whose row was corrupted,
// is refused here and SetMany never runs.
func (s *Service) validateBundle(p Persona) error {
	if cleanName(p.Name) == "" || strings.TrimSpace(p.Filler) == "" || strings.TrimSpace(p.Unavailable) == "" || strings.TrimSpace(p.Personality) == "" {
		return invalid("The stored persona is incomplete; edit it before selecting.")
	}
	if err := checkText(p.Personality, MaxPersonalityRunes, true, "personality prompt"); err != nil {
		return err
	}
	if err := checkText(p.Filler, MaxWordingRunes, false, "Filler phrase"); err != nil {
		return err
	}
	if err := checkText(p.Unavailable, MaxWordingRunes, false, "Unavailable message"); err != nil {
		return err
	}
	voice, ok := s.voices.Get(p.Voice)
	if !ok {
		return &Error{Kind: ErrVoiceUnavailable, Message: fmt.Sprintf("The voice %s isn't in the library.", p.Voice)}
	}
	if !voice.Ready() {
		if voice.Stale {
			return &Error{Kind: ErrVoiceUnavailable, Message: fmt.Sprintf("The %s voice needs re-exporting for the updated speech model, so I left everything as it was.", voice.Name)}
		}
		return &Error{Kind: ErrVoiceUnavailable, Message: fmt.Sprintf("The %s voice hasn't passed the model check, so I left everything as it was.", voice.Name)}
	}
	if _, err := s.voices.SpeechRef(voice.Name); err != nil {
		return &Error{Kind: ErrVoiceUnavailable, Message: fmt.Sprintf("The %s voice can't be used right now, so I left everything as it was.", voice.Name)}
	}
	return nil
}

// reload replaces the in-memory index from SQLite.
func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, voice, personality, filler, unavailable, created_at, updated_at FROM personas ORDER BY name`)
	if err != nil {
		return fmt.Errorf("persona: read library: %w", err)
	}
	defer rows.Close()

	indexed := make(map[string]Persona)
	for rows.Next() {
		var (
			p                Persona
			created, updated string
		)
		if err := rows.Scan(&p.Name, &p.Voice, &p.Personality, &p.Filler, &p.Unavailable, &created, &updated); err != nil {
			return fmt.Errorf("persona: read library row: %w", err)
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		p.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		indexed[p.Name] = p
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("persona: read library: %w", err)
	}
	s.mu.Lock()
	s.personas = indexed
	s.mu.Unlock()
	return nil
}

// invalid builds a validation sentinel with a user-safe sentence.
func invalid(message string) error {
	return &Error{Kind: ErrInvalidPersona, Message: message}
}

// cleanName collapses whitespace so a spoken name matches its stored spelling.
func cleanName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// cleanWording trims the spoken phrase without changing its inner spacing.
func cleanWording(text string) string {
	return strings.TrimSpace(text)
}

// checkName applies the bounds that keep a name usable in a URL and by voice.
func checkName(name string) error {
	if runes := []rune(name); len(runes) > MaxNameRunes {
		return invalid(fmt.Sprintf("A persona name is limited to %d characters.", MaxNameRunes))
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return invalid("A persona name can't contain control characters or slashes.")
		}
	}
	return nil
}

// checkText bounds one free-text field. The personality prompt may span lines;
// spoken wording may not, because the Filler and Unavailable message are
// single utterances.
func checkText(text string, limit int, multiline bool, label string) error {
	if runes := []rune(text); len(runes) > limit {
		return invalid(fmt.Sprintf("The %s is limited to %d characters.", label, limit))
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			if multiline {
				continue
			}
			return invalid(fmt.Sprintf("The %s must be one line.", label))
		}
		if r < 0x20 || r == 0x7f {
			return invalid(fmt.Sprintf("The %s contains a control character.", label))
		}
	}
	return nil
}

// sameWording compares two phrases after trimming, so a stray trailing space
// is not reported as a different bundle.
func sameWording(effective, persona string) bool {
	return strings.TrimSpace(effective) == strings.TrimSpace(persona)
}

// validateActiveSetting allows an empty marker and otherwise checks the same
// name bounds as a Persona record. A removed record is tolerated at read time.
func validateActiveSetting(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("want a string, got %T", value)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return checkName(text)
}

// stamp renders one time in the table's RFC3339 form.
func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
