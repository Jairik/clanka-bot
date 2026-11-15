// Package phrasebook owns the short spoken confirmation a Fast path Tool
// gives after it succeeds. Every Tool still renders its own full sentence; the
// phrase book replaces that sentence for the tools whose reply is only an
// acknowledgement ("Set.", "Playing.", "Done.") and leaves content-bearing
// replies (the time, the weather, a list) alone.
//
// There are two layers. The code default is the terse phrase shipped for each
// overridable Tool, so the assistant is brief even with no Persona selected.
// A Persona may replace any of them; the Agent path can generate the whole set
// from that Persona's personality prompt; the Phrase book page is where the
// owner reviews, edits or resets it. Generation applies immediately.
//
// The registry asks this package for a replacement through a hook installed by
// main, so no Tool knows about Personas or the phrase book.
package phrasebook

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
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// MaxPhraseRunes bounds one spoken phrase. The reply is a confirmation, not a
// paragraph: a long value is refused before it reaches the database or TTS.
const MaxPhraseRunes = 80

// Sources record where a phrase came from so the Dashboard can show it.
const (
	SourceDefault Source = "default"
	SourceManual  Source = "manual"
	SourceLLM     Source = "agent"
)

// Source is where a phrase was written.
type Source string

// Sources lists the writable sources, for validation and the page.
func Sources() []Source { return []Source{SourceManual, SourceLLM} }

// Valid reports whether s is a writable source.
func (s Source) Valid() bool { return s == SourceManual || s == SourceLLM }

// Entry is one Persona override.
type Entry struct {
	Persona   string
	Tool      string
	Phrase    string
	Source    Source
	UpdatedAt time.Time
}

// Slot is one overridable Tool as the page and the generator see it.
type Slot struct {
	Tool        string
	Group       string
	Description string
	Default     string
}

// Completer is the one-shot model seam. Agent implements it with the same
// provider, key and ZDR fields the conversational path uses.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// ErrUnknownTool means the Tool is not overridable.
var ErrUnknownTool = errors.New("phrasebook: tool is not overridable")

// ErrInvalidPhrase means the text is empty, over-long or not one line.
var ErrInvalidPhrase = errors.New("phrasebook: invalid phrase")

// ErrNoCompleter means generation was requested without an Agent provider.
var ErrNoCompleter = errors.New("phrasebook: no agent provider is configured")

// defaults is the shipped terse confirmation per overridable Tool. A Tool is
// overridable exactly when it appears here; content-bearing replies are
// deliberately absent so they can never be replaced by "Done.".
var defaults = map[string]string{
	"audio.volume_set":      "Set.",
	"audio.stop":            "Stopped.",
	"timer.set":             "Set.",
	"timer.cancel":          "Cancelled.",
	"alarm.set":             "Set.",
	"alarm.cancel":          "Cancelled.",
	"alarm.update":          "Updated.",
	"quiet.set":             "Quiet.",
	"quiet.off":             "Loud.",
	"quiet.dnd":             "Do not disturb.",
	"quiet.awake":           "Awake.",
	"voice.set":             "Done.",
	"voice.rate":            "Done.",
	"reminder.set":          "Set.",
	"reminder.cancel":       "Cancelled.",
	"notify.phone":          "Sent.",
	"persona.switch":        "Switched.",
	"spotify.play":          "Playing.",
	"spotify.pause":         "Paused.",
	"spotify.next":          "Next.",
	"spotify.volume":        "Done.",
	"spotify.play_playlist": "Playing.",
	"spotify.shuffle":       "Shuffled.",
	"spotify.repeat":        "Repeating.",
	"spotify.previous":      "Previous.",
	"spotify.queue_add":     "Queued.",
	"spotify.like":          "Liked.",
	"note.remember":         "Saved.",
	"note.forget":           "Forgotten.",
	"list.add":              "Added.",
	"list.remove":           "Removed.",
	"lights.power":          "Done.",
	"lights.brightness":     "Done.",
	"lights.color":          "Done.",
	"lights.mode":           "Done.",
}

// Overridable reports whether the phrase book may replace this Tool's reply.
func Overridable(name string) bool {
	_, ok := defaults[name]
	return ok
}

// Default returns the shipped terse phrase for a Tool.
func Default(name string) (string, bool) {
	text, ok := defaults[name]
	return text, ok
}

// Service is the persisted phrase book plus the live renderer.
type Service struct {
	db     *sql.DB
	active func() string
	logger *slog.Logger

	mu        sync.RWMutex
	byPersona map[string]map[string]Entry
}

// New opens the phrase book from the one SQLite store. active resolves the
// selected Persona per call, so selecting one applies to the next reply
// without rebuilding anything.
func New(ctx context.Context, st *store.Store, active func() string, logger *slog.Logger) (*Service, error) {
	if st == nil || st.DB() == nil {
		return nil, errors.New("phrasebook: store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		db:        st.DB(),
		active:    active,
		logger:    logger,
		byPersona: make(map[string]map[string]Entry),
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT persona, tool, phrase, source, updated_at FROM persona_phrases`)
	if err != nil {
		return fmt.Errorf("phrasebook: load: %w", err)
	}
	defer rows.Close()
	loaded := make(map[string]map[string]Entry)
	for rows.Next() {
		var (
			persona, name, text, source string
			updatedMs                   int64
		)
		if err := rows.Scan(&persona, &name, &text, &source, &updatedMs); err != nil {
			return fmt.Errorf("phrasebook: scan: %w", err)
		}
		key := personaKey(persona)
		if loaded[key] == nil {
			loaded[key] = make(map[string]Entry)
		}
		loaded[key][name] = Entry{
			Persona:   persona,
			Tool:      name,
			Phrase:    text,
			Source:    Source(source),
			UpdatedAt: time.UnixMilli(updatedMs).UTC(),
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("phrasebook: load: %w", err)
	}
	s.mu.Lock()
	s.byPersona = loaded
	s.mu.Unlock()
	return nil
}

func personaKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Entries returns one Persona's overrides in Tool-name order.
func (s *Service) Entries(persona string) []Entry {
	key := personaKey(persona)
	if key == "" {
		return nil
	}
	s.mu.RLock()
	set := s.byPersona[key]
	out := make([]Entry, 0, len(set))
	for _, e := range set {
		out = append(out, e)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// Get returns one Persona's override for a Tool.
func (s *Service) Get(persona, name string) (Entry, bool) {
	key := personaKey(persona)
	if key == "" {
		return Entry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byPersona[key][name]
	return e, ok
}

// Set writes one override after validating the Tool and the text.
func (s *Service) Set(ctx context.Context, persona, name, text string, source Source) (Entry, error) {
	persona = strings.TrimSpace(persona)
	if persona == "" {
		return Entry{}, fmt.Errorf("%w: persona is required", ErrInvalidPhrase)
	}
	if !Overridable(name) {
		return Entry{}, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
	text, err := cleanPhrase(text)
	if err != nil {
		return Entry{}, err
	}
	if !source.Valid() {
		source = SourceManual
	}
	now := time.Now().UTC().Truncate(time.Second)
	// The column stores the folded lookup key, not the display spelling: the
	// personas table matches case-insensitively with EqualFold, while SQLite's
	// NOCASE only folds ASCII. Storing the key keeps the database and the
	// in-memory map on exactly the same row for any Unicode name.
	key := personaKey(persona)
	entry := Entry{Persona: key, Tool: name, Phrase: text, Source: source, UpdatedAt: now}
	// Hold the lock across the write and the memory swap, so two tabs cannot
	// commit in one order and update the map in the other.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO persona_phrases (persona, tool, phrase, source, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(persona, tool) DO UPDATE SET
		   phrase = excluded.phrase,
		   source = excluded.source,
		   updated_at = excluded.updated_at`,
		key, name, text, string(source), now.UnixMilli()); err != nil {
		return Entry{}, fmt.Errorf("phrasebook: save %s: %w", name, err)
	}
	if s.byPersona[key] == nil {
		s.byPersona[key] = make(map[string]Entry)
	}
	s.byPersona[key][name] = entry
	s.logger.Info("phrase saved", "persona", key, "tool", name, "source", source)
	return entry, nil
}

// Replace writes a whole generated set for one Persona in one transaction.
// It replaces every existing override, including manual edits, so the page
// asks before it runs. The database is only touched when every phrase passes
// validation.
func (s *Service) Replace(ctx context.Context, persona string, phrases map[string]string, source Source) ([]Entry, error) {
	persona = strings.TrimSpace(persona)
	if persona == "" {
		return nil, fmt.Errorf("%w: persona is required", ErrInvalidPhrase)
	}
	if !source.Valid() {
		source = SourceLLM
	}
	clean := make(map[string]string, len(phrases))
	for name, text := range phrases {
		if !Overridable(name) {
			return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
		}
		value, err := cleanPhrase(text)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		clean[name] = value
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("%w: no phrases", ErrInvalidPhrase)
	}
	now := time.Now().UTC().Truncate(time.Second)
	key := personaKey(persona)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("phrasebook: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM persona_phrases WHERE persona = ?`, key); err != nil {
		return nil, fmt.Errorf("phrasebook: clear: %w", err)
	}
	names := make([]string, 0, len(clean))
	for name := range clean {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO persona_phrases (persona, tool, phrase, source, updated_at) VALUES (?, ?, ?, ?, ?)`,
			key, name, clean[name], string(source), now.UnixMilli()); err != nil {
			return nil, fmt.Errorf("phrasebook: save %s: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("phrasebook: commit: %w", err)
	}
	set := make(map[string]Entry, len(clean))
	for name, text := range clean {
		set[name] = Entry{Persona: key, Tool: name, Phrase: text, Source: source, UpdatedAt: now}
	}
	s.byPersona[key] = set
	s.logger.Info("phrase book replaced", "persona", key, "phrases", len(clean), "source", source)
	entries := make([]Entry, 0, len(set))
	for _, entry := range set {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Tool < entries[j].Tool })
	return entries, nil
}

// Delete removes one override, restoring the shipped default.
func (s *Service) Delete(ctx context.Context, persona, name string) error {
	persona = strings.TrimSpace(persona)
	if persona == "" {
		return fmt.Errorf("%w: persona is required", ErrInvalidPhrase)
	}
	key := personaKey(persona)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM persona_phrases WHERE persona = ? AND tool = ?`, key, name); err != nil {
		return fmt.Errorf("phrasebook: delete %s: %w", name, err)
	}
	delete(s.byPersona[key], name)
	return nil
}

// Phrase resolves the reply for a Tool: the active Persona's override, then
// the shipped default. The bool is false for a content-bearing Tool, which the
// caller must leave untouched.
func (s *Service) Phrase(name string) (string, bool) {
	dflt, ok := Default(name)
	if !ok {
		return "", false
	}
	if s == nil || s.active == nil {
		return dflt, true
	}
	persona := strings.TrimSpace(s.active())
	if persona == "" {
		return dflt, true
	}
	if e, ok := s.Get(persona, name); ok && e.Phrase != "" {
		return e.Phrase, true
	}
	return dflt, true
}

// Render is the registry hook: it replaces an overridable Tool's rendered
// reply and passes every other Tool through unchanged.
func (s *Service) Render(name, fallback string) string {
	if !Overridable(name) {
		return fallback
	}
	if text, ok := s.Phrase(name); ok && text != "" {
		return text
	}
	return fallback
}

// Slots returns every overridable Tool that has a Speak renderer, with the
// shipped default and the Tool description the generator uses for context.
func Slots(src interface {
	Info(name string) (tool.Info, bool)
}) []Slot {
	if src == nil {
		return nil
	}
	names := make([]string, 0, len(defaults))
	for name := range defaults {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Slot, 0, len(names))
	for _, name := range names {
		info, ok := src.Info(name)
		if !ok || !info.HasSpeak {
			continue
		}
		group := name
		if i := strings.Index(name, "."); i > 0 {
			group = name[:i]
		}
		out = append(out, Slot{
			Tool:        name,
			Group:       group,
			Description: info.Description,
			Default:     defaults[name],
		})
	}
	return out
}

// Generate asks the one-shot model for a whole in-character phrase set. It
// returns only validated phrases for known overridable Tools; an omitted Tool
// keeps its shipped default.
func Generate(ctx context.Context, persona, personality string, slots []Slot, completer Completer) (map[string]string, error) {
	if completer == nil {
		return nil, ErrNoCompleter
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("%w: no slots", ErrInvalidPhrase)
	}
	var tools strings.Builder
	for _, slot := range slots {
		fmt.Fprintf(&tools, "- %s: %s (default %q)\n", slot.Tool, slot.Description, slot.Default)
	}
	system := "You write the shortest possible spoken confirmations for a private voice assistant. " +
		"Return one JSON object and nothing else: no prose, no markdown, no code fence."
	user := fmt.Sprintf(`Persona name: %s
Persona personality:
%s

Write the reply the assistant speaks after each successful tool call, in this persona's voice.

Rules:
- 1 to 4 words per reply. Never more than 4 words.
- Stay in character. Profanity is allowed when the personality uses it.
- The reply confirms the action only. Never invent facts, numbers, names or offers.
- Plain text for text-to-speech: no quotes, emoji, markdown, placeholders or symbols.
- Return every key exactly as listed, with a string value.

Tools:
%s
Return one JSON object mapping every tool name to its reply.`, strings.TrimSpace(persona), strings.TrimSpace(personality), tools.String())
	raw, err := completer.Complete(ctx, system, user)
	if err != nil {
		return nil, fmt.Errorf("phrasebook: generate: %w", err)
	}
	parsed, err := parseJSONObject(raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(parsed))
	for name, value := range parsed {
		if !Overridable(name) {
			continue
		}
		text, err := cleanPhrase(value)
		if err != nil {
			continue
		}
		out[name] = text
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the model returned no usable phrases", ErrInvalidPhrase)
	}
	return out, nil
}

// parseJSONObject accepts a bare object or one wrapped in a code fence, so a
// provider that ignores "JSON only" still works.
func parseJSONObject(raw string) (map[string]string, error) {
	text := strings.TrimSpace(raw)
	if start, end := strings.Index(text, "{"), strings.LastIndex(text, "}"); start >= 0 && end > start {
		text = text[start : end+1]
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("phrasebook: decode model reply: %w", err)
	}
	return out, nil
}

// cleanPhrase trims and bounds one phrase. Newlines are refused because the
// text is spoken and shown as one row.
func cleanPhrase(text string) (string, error) {
	text = strings.TrimSpace(text)
	// Strip only a matched surrounding quote pair. A manual phrase such as
	// "Rockin'" keeps its apostrophe; the model's quoted output loses only the
	// wrapper.
	if len(text) >= 2 && (text[0] == '"' || text[0] == '\'') && text[len(text)-1] == text[0] {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	if text == "" {
		return "", fmt.Errorf("%w: phrase is empty", ErrInvalidPhrase)
	}
	if utf8.RuneCountInString(text) > MaxPhraseRunes {
		return "", fmt.Errorf("%w: phrase is longer than %d characters", ErrInvalidPhrase, MaxPhraseRunes)
	}
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("%w: phrase must be one line", ErrInvalidPhrase)
	}
	return text, nil
}
