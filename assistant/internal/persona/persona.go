// Package persona owns the persistent Persona library and the atomic switch of
// one bundle: voice, personality prompt, Agent Filler and Unavailable wording.
// The personality prompt is read only by the Agent path's single prompt builder
// (ADR-0002); selecting a Persona writes the same live settings the voice and
// wording Tools use, in one transaction.
package persona

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/conversation"
	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
	toolconv "github.com/Jairik/clanka-bot/assistant/tools/conversation"
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
	// MaxPhrases bounds one optional Persona phrase list. A Persona adds
	// claims to every request's routing table, so a pasted block cannot grow
	// it without limit.
	MaxPhrases = 32
)

// ConversationStartTool and ConversationEndTool are the Control phrase owners
// a Persona's optional trigger phrases add to. They are the conversation
// Tools' own names, so a rename cannot leave routing pointed at a dead Tool.
const (
	ConversationStartTool = toolconv.StartName
	ConversationEndTool   = toolconv.EndName
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
	// Clarify and CueWords are optional live wording. An empty field resets
	// the live setting to its global layer, so switching to a Persona without
	// them restores the default rather than keeping the previous Persona's.
	Clarify  string
	CueWords []string
	// StartPhrases and EndPhrases are optional Conversation trigger phrases.
	// They add to the global Control phrases on the next request and never
	// replace them. They stay on the record, so no settings transaction owns
	// them.
	StartPhrases []string
	EndPhrases   []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Input is a create or edit form. It contains no times; the service owns them.
type Input struct {
	Name         string
	Voice        string
	Personality  string
	Filler       string
	Unavailable  string
	Clarify      string
	CueWords     []string
	StartPhrases []string
	EndPhrases   []string
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
	EffectiveClarify      string
	EffectiveCueWords     []string
	VoiceDiffers          bool
	FillerDiffers         bool
	UnavailableDiffers    bool
	ClarifyDiffers        bool
	CueWordsDiffers       bool
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
			settings.WithDescription("Active Persona name. Selecting a Persona writes voice.name, agent.filler, agent.unavailable_message and the optional clarify line and cue words with it in one transaction; resetting this key leaves those applied values alone."),
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
		Name:         clean.Name,
		Voice:        clean.Voice,
		Personality:  clean.Personality,
		Filler:       clean.Filler,
		Unavailable:  clean.Unavailable,
		Clarify:      clean.Clarify,
		CueWords:     clean.CueWords,
		StartPhrases: clean.StartPhrases,
		EndPhrases:   clean.EndPhrases,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO personas (name, voice, personality, filler, unavailable, clarify, cue_words, start_phrases, end_phrases, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Voice, p.Personality, p.Filler, p.Unavailable,
		p.Clarify, encodeList(p.CueWords), encodeList(p.StartPhrases), encodeList(p.EndPhrases),
		stamp(p.CreatedAt), stamp(p.UpdatedAt)); err != nil {
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
	updated.Clarify = clean.Clarify
	updated.CueWords = clean.CueWords
	updated.StartPhrases = clean.StartPhrases
	updated.EndPhrases = clean.EndPhrases
	updated.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx,
		`UPDATE personas SET voice = ?, personality = ?, filler = ?, unavailable = ?, clarify = ?, cue_words = ?, start_phrases = ?, end_phrases = ?, updated_at = ? WHERE name = ?`,
		updated.Voice, updated.Personality, updated.Filler, updated.Unavailable,
		updated.Clarify, encodeList(updated.CueWords), encodeList(updated.StartPhrases), encodeList(updated.EndPhrases),
		stamp(updated.UpdatedAt), existing.Name); err != nil {
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
	values := map[string]any{
		voicelib.KeyName:       p.Voice,
		wording.FillerKey:      p.Filler,
		wording.UnavailableKey: p.Unavailable,
		ActiveKey:              p.Name,
	}
	// An empty optional field clears the previous Persona's applied override,
	// so switching Personas restores the global value. A manual override that
	// no Persona claims is the owner's global value and is left alone.
	previousName := strings.TrimSpace(svc.String(ActiveKey))
	previous, hasPrevious := s.Get(previousName)
	reselect := hasPrevious && strings.EqualFold(previousName, p.Name)
	var resets []string
	applyOptionalString(svc, values, &resets, wording.ClarifyKey, p.Clarify, previous.Clarify, hasPrevious, reselect)
	applyOptionalList(svc, values, &resets, conversation.CueWordsKey, p.CueWords, previous.CueWords, hasPrevious, reselect)
	if err := svc.SetManyReset(ctx, values, resets); err != nil {
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
	current.EffectiveClarify, current.ClarifyDiffers = optionalStringDrift(svc, wording.ClarifyKey, p.Clarify)
	current.EffectiveCueWords, current.CueWordsDiffers = optionalListDrift(svc, conversation.CueWordsKey, p.CueWords)
	current.Exact = !current.VoiceDiffers && !current.FillerDiffers && !current.UnavailableDiffers &&
		!current.ClarifyDiffers && !current.CueWordsDiffers
	return current, true
}

// optionalStringDrift compares one optional field with the live setting. An
// empty Persona field keeps the global value, but a stored override is still
// drift: selecting the Persona again would clear it.
func optionalStringDrift(svc *settings.Service, key, persona string) (string, bool) {
	value, ok := svc.Get(key)
	if !ok {
		return "", strings.TrimSpace(persona) != ""
	}
	effective, _ := value.Value.(string)
	if strings.TrimSpace(persona) == "" {
		return effective, value.Source == settings.SourceOverride
	}
	return effective, !sameWording(effective, persona)
}

// optionalListDrift is optionalStringDrift for a list-valued setting.
func optionalListDrift(svc *settings.Service, key string, persona []string) ([]string, bool) {
	value, ok := svc.Get(key)
	if !ok {
		return nil, len(persona) > 0
	}
	effective, _ := value.Value.([]string)
	if len(persona) == 0 {
		return effective, value.Source == settings.SourceOverride
	}
	return effective, !sameList(effective, persona)
}

// TriggerPhrases returns the active Persona's optional Conversation trigger
// phrases, keyed by Tool name. The live wording source merges them into the
// global Control phrases on every request, so a selection never rewrites the
// shared lists. An empty result adds nothing.
func (s *Service) TriggerPhrases() map[string][]string {
	svc := s.settingsService()
	if svc == nil {
		return nil
	}
	name := strings.TrimSpace(svc.String(ActiveKey))
	if name == "" {
		return nil
	}
	p, ok := s.Get(name)
	if !ok {
		return nil
	}
	out := make(map[string][]string, 2)
	if len(p.StartPhrases) > 0 {
		out[ConversationStartTool] = append([]string(nil), p.StartPhrases...)
	}
	if len(p.EndPhrases) > 0 {
		out[ConversationEndTool] = append([]string(nil), p.EndPhrases...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
		Name:         cleanName(in.Name),
		Voice:        strings.TrimSpace(in.Voice),
		Personality:  strings.TrimSpace(in.Personality),
		Filler:       cleanWording(in.Filler),
		Unavailable:  cleanWording(in.Unavailable),
		Clarify:      cleanWording(in.Clarify),
		CueWords:     cleanList(in.CueWords),
		StartPhrases: cleanList(in.StartPhrases),
		EndPhrases:   cleanList(in.EndPhrases),
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
	if clean.Clarify != "" {
		if err := checkText(clean.Clarify, MaxWordingRunes, false, "clarify line"); err != nil {
			return Input{}, err
		}
	}
	if err := checkList(clean.CueWords, "cue word"); err != nil {
		return Input{}, err
	}
	if err := checkList(clean.StartPhrases, "start trigger phrase"); err != nil {
		return Input{}, err
	}
	if err := checkList(clean.EndPhrases, "end trigger phrase"); err != nil {
		return Input{}, err
	}
	// A save must not create a phrase that another Tool's Control list already
	// owns. Selection does not repeat this check: a later global edit must not
	// make an existing Persona unselectable; the routing overlay keeps the
	// global phrase in that case.
	if err := s.validateTriggerPhrases(clean.StartPhrases, clean.EndPhrases); err != nil {
		return Input{}, err
	}
	if err := s.validateBundle(Persona{
		Name:         clean.Name,
		Voice:        clean.Voice,
		Personality:  clean.Personality,
		Filler:       clean.Filler,
		Unavailable:  clean.Unavailable,
		Clarify:      clean.Clarify,
		CueWords:     clean.CueWords,
		StartPhrases: clean.StartPhrases,
		EndPhrases:   clean.EndPhrases,
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
	if p.Clarify != "" {
		if err := checkText(p.Clarify, MaxWordingRunes, false, "clarify line"); err != nil {
			return err
		}
	}
	if err := checkList(p.CueWords, "cue word"); err != nil {
		return err
	}
	if err := checkList(p.StartPhrases, "start trigger phrase"); err != nil {
		return err
	}
	if err := checkList(p.EndPhrases, "end trigger phrase"); err != nil {
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
		`SELECT name, voice, personality, filler, unavailable, clarify, cue_words, start_phrases, end_phrases, created_at, updated_at FROM personas ORDER BY name`)
	if err != nil {
		return fmt.Errorf("persona: read library: %w", err)
	}
	defer rows.Close()

	indexed := make(map[string]Persona)
	for rows.Next() {
		var (
			p                    Persona
			cueWords, start, end string
			created, updated     string
		)
		if err := rows.Scan(&p.Name, &p.Voice, &p.Personality, &p.Filler, &p.Unavailable,
			&p.Clarify, &cueWords, &start, &end, &created, &updated); err != nil {
			return fmt.Errorf("persona: read library row: %w", err)
		}
		p.CueWords = decodeList(cueWords)
		p.StartPhrases = decodeList(start)
		p.EndPhrases = decodeList(end)
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

// cleanList trims each entry and drops blank ones. A form textarea sends one
// phrase per line, and a trailing newline is formatting, not a value.
func cleanList(list []string) []string {
	out := make([]string, 0, len(list))
	for _, text := range list {
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// checkList bounds every phrase in one optional list, including the count.
func checkList(list []string, label string) error {
	if len(list) > MaxPhrases {
		return invalid(fmt.Sprintf("A phrase list is limited to %d entries.", MaxPhrases))
	}
	for _, text := range list {
		if err := checkText(text, MaxWordingRunes, false, label); err != nil {
			return err
		}
	}
	return nil
}

// encodeList stores a string list as JSON text. An empty list keeps the
// column's default form so a row always decodes.
func encodeList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// decodeList reads the JSON text stored by encodeList. A corrupt value reads
// as empty rather than making the whole library unreadable.
func decodeList(raw string) []string {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	return cleanList(list)
}

// applyOptionalString writes a non-empty optional field. An empty one clears
// the live override only when it is the previous Persona's applied value, or
// when the same Persona is selected again after an edit; a manual override
// that no Persona claims is left alone. A build without the setting has
// nothing to apply.
func applyOptionalString(svc *settings.Service, values map[string]any, resets *[]string, key, value, previous string, hasPrevious, reselect bool) {
	current, ok := svc.Get(key)
	if !ok {
		return
	}
	if value = strings.TrimSpace(value); value != "" {
		values[key] = value
		return
	}
	if current.Source != settings.SourceOverride {
		return
	}
	if reselect {
		*resets = append(*resets, key)
		return
	}
	effective, _ := current.Value.(string)
	if hasPrevious && strings.TrimSpace(previous) != "" && sameWording(effective, previous) {
		*resets = append(*resets, key)
	}
}

// applyOptionalList is applyOptionalString for a list-valued setting.
func applyOptionalList(svc *settings.Service, values map[string]any, resets *[]string, key string, list, previous []string, hasPrevious, reselect bool) {
	current, ok := svc.Get(key)
	if !ok {
		return
	}
	if len(list) > 0 {
		values[key] = append([]string(nil), list...)
		return
	}
	if current.Source != settings.SourceOverride {
		return
	}
	if reselect {
		*resets = append(*resets, key)
		return
	}
	effective, _ := current.Value.([]string)
	if hasPrevious && len(previous) > 0 && sameList(effective, previous) {
		*resets = append(*resets, key)
	}
}

// validateTriggerPhrases checks the optional Conversation trigger phrases. They
// add to the global Control phrases, so a phrase may not be repeated or already
// belong to another Tool's list; otherwise it would be ambiguous or shadow an
// existing route.
func (s *Service) validateTriggerPhrases(start, end []string) error {
	groups := []struct {
		label string
		tool  string
		list  []string
	}{
		{"start", ConversationStartTool, start},
		{"end", ConversationEndTool, end},
	}
	seen := make(map[string]string, len(start)+len(end))
	for _, group := range groups {
		for _, text := range group.list {
			key := phrase.Normalize(text)
			if key == "" {
				return invalid("A conversation trigger phrase must not be blank.")
			}
			if prev, ok := seen[key]; ok {
				if prev == group.label {
					return invalid(fmt.Sprintf("The trigger phrase %q is repeated.", text))
				}
				return invalid(fmt.Sprintf("The trigger phrase %q cannot be both a start and an end phrase.", text))
			}
			seen[key] = group.label
		}
	}
	svc := s.settingsService()
	if svc == nil {
		return nil
	}
	owners, ambiguous := controlPhraseOwners(svc)
	for _, group := range groups {
		for _, text := range group.list {
			key := phrase.Normalize(text)
			owner, claimed := owners[key]
			if !claimed {
				continue
			}
			if ambiguous[key] || owner != group.tool {
				return invalid(fmt.Sprintf("The trigger phrase %q is already a Control phrase for %s.", text, owner))
			}
		}
	}
	return nil
}

// controlPhraseOwners indexes every global Control phrase by its normalized
// text. A phrase claimed by two owners is marked ambiguous rather than letting
// one of them win.
func controlPhraseOwners(svc *settings.Service) (map[string]string, map[string]bool) {
	owners := make(map[string]string)
	ambiguous := make(map[string]bool)
	for _, value := range svc.List() {
		var owner string
		switch {
		case strings.HasPrefix(value.Key, "tools.") && strings.HasSuffix(value.Key, ".control_phrases"):
			owner = strings.TrimSuffix(strings.TrimPrefix(value.Key, "tools."), ".control_phrases")
		case strings.HasPrefix(value.Key, "focus.") && strings.HasSuffix(value.Key, ".phrases"):
			owner = value.Key
		default:
			continue
		}
		list, _ := value.Value.([]string)
		for _, text := range list {
			key := phrase.Normalize(text)
			if key == "" {
				continue
			}
			if prev, ok := owners[key]; ok && prev != owner {
				ambiguous[key] = true
			}
			owners[key] = owner
		}
	}
	return owners, ambiguous
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

// sameList compares two cue-word lists in order. An empty Persona list means
// the global value applies, so the caller decides when there is anything to
// compare.
func sameList(effective, persona []string) bool {
	if len(effective) != len(persona) {
		return false
	}
	for i := range effective {
		if strings.TrimSpace(effective[i]) != strings.TrimSpace(persona[i]) {
			return false
		}
	}
	return true
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
