// Package voicelib owns the voice library and the persistent voice/rate
// selection. Custom voices are pre-exported Pocket TTS .safetensors states in
// one folder on the data volume; the folder is mounted read-only into tts, so
// only the file and its validation revision are stored here. Built-in voices
// are the ones the TTS service advertises as warm.
package voicelib

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// Settings keys shared by the voice Tools and the Dashboard.
const (
	// KeyName is the persistent active voice. A stored override wins over
	// config.yaml and the code default.
	KeyName = "voice.name"
	// KeyRate is the speech-rate multiplier: 1.0 is normal, 2.0 is twice as
	// fast. It is a multiplier, not words per minute.
	KeyRate = "voice.rate"
)

// Rate bounds and the default. Linear resampling keeps artefacts bounded;
// values outside this range sound metallic rather than merely fast or slow.
const (
	DefaultRate = 1.0
	MinRate     = 0.5
	MaxRate     = 2.0
)

// Errors returned by SpeechRef for a library entry that must not be used.
var (
	ErrUnknownVoice = errors.New("voice: unknown voice")
	ErrStaleVoice   = errors.New("voice: voice state was validated against a different model revision")
	ErrNotValidated = errors.New("voice: voice state has not passed the model smoke test")
)

// Voice is one library entry. Built-ins live in the TTS image; custom entries
// are files in Dir.
type Voice struct {
	Name              string
	Builtin           bool
	File              string
	Path              string
	Size              int64
	AddedAt           time.Time
	ValidatedRevision string
	ValidatedAt       time.Time
	Stale             bool
	LastError         string
	Note              string

	// modTime is the file's modification time in Unix nanoseconds. It is not
	// part of the public list; it lets a rescan tell an unchanged file from a
	// replaced export with the same name.
	modTime int64
}

// Ready reports whether the entry can be selected. A built-in is always ready;
// a custom state needs a successful smoke test against the current revision and
// must not be stale.
func (v Voice) Ready() bool {
	if v.Builtin {
		return true
	}
	return !v.Stale && strings.TrimSpace(v.ValidatedRevision) != ""
}

// Status is the short state the list Tool and a future Dashboard page show.
func (v Voice) Status() string {
	switch {
	case v.Builtin:
		return "builtin"
	case v.Stale:
		return "stale"
	case v.ValidatedRevision == "":
		return "unvalidated"
	default:
		return "ready"
	}
}

// Config is the service's one-time setup.
type Config struct {
	// Dir is the assistant-side library folder holding .safetensors files.
	Dir string
	// TTSDir is the path the tts container sees the same folder at. The
	// assistant sends "<TTSDir>/<file>" to TTS; it never sends a host path.
	TTSDir string
	// Default is the configured fallback voice, normally tts.voice.
	Default string
	// Builtins are the voices the TTS service advertises as warm.
	Builtins []string
	Logger   *slog.Logger
}

// Probe is the TTS boundary the library uses. Revision reports the running
// model revision without generating audio; Validate loads one state file and
// performs the real model smoke test, returning the revision it passed against.
type Probe interface {
	Revision(ctx context.Context) (string, error)
	Validate(ctx context.Context, remotePath string) (revision string, err error)
}

// Fallback reports that the persisted active voice could not be used and the
// service replaced it with the default built-in.
type Fallback struct {
	From string
	To   string
}

// Service is the in-process voice library and selection seam.
type Service struct {
	db          *sql.DB
	dir         string
	ttsDir      string
	defaultName string
	logger      *slog.Logger

	mu       sync.RWMutex
	builtins []string
	voices   map[string]Voice
	revision string
	settings *settings.Service

	// importMu serialises upload staging, publish and rollback. The scan skips
	// dot-prefixed staging files, but two same-name imports would still race
	// their backup renames without this lock.
	importMu sync.Mutex
}

// New opens the library and creates its folder. It does not scan or validate
// files; main calls Rescan after the settings service exists, so the first
// scan can fall back an unusable active voice in the same startup.
func New(ctx context.Context, st *store.Store, cfg Config) (*Service, error) {
	if st == nil || st.DB() == nil {
		return nil, errors.New("voice: store is required")
	}
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		return nil, errors.New("voice: library directory is required")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("voice: resolve library directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("voice: create library directory: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// A crash between staging and publish can leave hidden temp or backup
	// files. No upload can be in flight before New returns, so remove them now;
	// the scanner would ignore them, but they would otherwise stay forever.
	cleanupStaging(absolute, logger)
	ttsDir := strings.TrimSpace(cfg.TTSDir)
	if ttsDir == "" {
		ttsDir = "/voices"
	}

	s := &Service{
		db:       st.DB(),
		dir:      absolute,
		ttsDir:   path.Clean(ttsDir),
		logger:   logger,
		builtins: builtinNames(cfg.Builtins, cfg.Default),
		voices:   make(map[string]Voice),
	}
	s.defaultName = s.pickDefault(cfg.Default)
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// builtinNames deduplicates the advertised names, sorts them for a stable list,
// and makes sure the configured default is present even when the TTS service
// reported no voices yet.
func builtinNames(names []string, fallback string) []string {
	seen := make(map[string]bool, len(names)+1)
	out := make([]string, 0, len(names)+1)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	add(fallback)
	for _, name := range names {
		add(name)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// pickDefault prefers the configured voice when it is a known built-in, then
// the first built-in. The configured value stays selectable because
// builtinNames always inserts it.
func (s *Service) pickDefault(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return configured
	}
	if len(s.builtins) > 0 {
		return s.builtins[0]
	}
	return "alba"
}

// Dir returns the assistant-side library folder.
func (s *Service) Dir() string { return s.dir }

// TTSDir returns the path the tts container sees the library at.
func (s *Service) TTSDir() string { return s.ttsDir }

// DefaultName is the built-in fallback voice.
func (s *Service) DefaultName() string { return s.defaultName }

// Revision is the last TTS model revision observed, or "" when TTS has never
// answered.
func (s *Service) Revision() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

// Bind installs the one settings service shared with the Dashboard and the
// voice Tools. It is late-bound because the settings definitions need the Tool
// registry first.
func (s *Service) Bind(svc *settings.Service) { s.settings = svc }

// Candidates returns every selectable name for the shared name matcher, sorted
// case-insensitively. It includes stale entries deliberately: the matcher may
// resolve them and voice.set then reports that the voice needs re-exporting,
// rather than pretending the name does not exist.
func (s *Service) Candidates() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]string(nil), s.builtins...)
	for name := range s.voices {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// List returns built-ins first, then custom files in name order.
func (s *Service) List() []Voice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Voice, 0, len(s.builtins)+len(s.voices))
	for _, name := range s.builtins {
		out = append(out, Voice{
			Name:              name,
			Builtin:           true,
			ValidatedRevision: s.revision,
		})
	}
	custom := make([]Voice, 0, len(s.voices))
	for _, voice := range s.voices {
		custom = append(custom, voice)
	}
	sort.Slice(custom, func(i, j int) bool { return strings.ToLower(custom[i].Name) < strings.ToLower(custom[j].Name) })
	return append(out, custom...)
}

// Get returns one entry by name. Matching is case-insensitive because a stored
// setting may predate a library rename that only changed case.
func (s *Service) Get(name string) (Voice, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Voice{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, builtin := range s.builtins {
		if strings.EqualFold(builtin, name) {
			return Voice{Name: builtin, Builtin: true, ValidatedRevision: s.revision}, true
		}
	}
	if voice, ok := s.voices[name]; ok {
		return voice, true
	}
	for key, voice := range s.voices {
		if strings.EqualFold(key, name) {
			return voice, true
		}
	}
	return Voice{}, false
}

// Active returns the effective persisted voice, falling back to the default
// name when the setting is missing or points at a removed entry.
func (s *Service) Active() (Voice, bool) {
	name := s.settingsString(KeyName)
	if name == "" {
		name = s.defaultName
	}
	if voice, ok := s.Get(name); ok {
		return voice, true
	}
	voice, ok := s.Get(s.defaultName)
	return voice, ok
}

// SpeechRef resolves a library entry to the value the Wyoming request carries:
// a built-in name, or the path inside the tts container for a custom state.
func (s *Service) SpeechRef(name string) (string, error) {
	voice, ok := s.Get(name)
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownVoice, name)
	}
	if voice.Builtin {
		return voice.Name, nil
	}
	if voice.Stale {
		return "", fmt.Errorf("%w: %q", ErrStaleVoice, voice.Name)
	}
	if strings.TrimSpace(voice.ValidatedRevision) == "" {
		return "", fmt.Errorf("%w: %q", ErrNotValidated, voice.Name)
	}
	return path.Join(s.ttsDir, voice.File), nil
}

// ActiveSpeech resolves the persisted voice and rate together, for the speech
// speaker's per-request source. It reports false when no usable entry exists.
func (s *Service) ActiveSpeech() (string, float64, bool) {
	voice, ok := s.Active()
	if !ok {
		return "", DefaultRate, false
	}
	ref, err := s.SpeechRef(voice.Name)
	if err != nil {
		return "", DefaultRate, false
	}
	return ref, s.Rate(), true
}

// Rate returns the effective speech-rate multiplier, defaulting when the
// setting is absent.
func (s *Service) Rate() float64 {
	if s.settings == nil {
		return DefaultRate
	}
	return validRate(s.settings.Float(KeyRate))
}

// SetActive persists one voice as the shared override. It refuses entries that
// have not passed the model smoke test, so a copied file cannot be selected
// just because its header parsed.
func (s *Service) SetActive(ctx context.Context, name string) (Voice, error) {
	if s.settings == nil {
		return Voice{}, errors.New("voice: settings service is not bound")
	}
	voice, ok := s.Get(name)
	if !ok {
		return Voice{}, fmt.Errorf("%w %q", ErrUnknownVoice, name)
	}
	if !voice.Ready() {
		if voice.Stale {
			return Voice{}, fmt.Errorf("%w: %q", ErrStaleVoice, voice.Name)
		}
		return Voice{}, fmt.Errorf("%w: %q", ErrNotValidated, voice.Name)
	}
	if _, err := s.settings.Set(ctx, KeyName, voice.Name); err != nil {
		return Voice{}, err
	}
	return voice, nil
}

// SetRate persists a clamped, rounded multiplier and returns the stored value.
func (s *Service) SetRate(ctx context.Context, rate float64) (float64, error) {
	if s.settings == nil {
		return DefaultRate, errors.New("voice: settings service is not bound")
	}
	rate = roundRate(clampRate(rate))
	if _, err := s.settings.Set(ctx, KeyRate, rate); err != nil {
		return DefaultRate, err
	}
	return rate, nil
}

// EnsureActive replaces an unusable persisted voice with the default built-in.
// It returns the change so main can make exactly one Announcement; a nil
// Fallback means nothing changed or the active voice is fine. A TTS failure
// never reaches this path: Rescan marks stale entries and leaves valid ones
// alone.
func (s *Service) EnsureActive(ctx context.Context) (*Fallback, error) {
	if s.settings == nil {
		return nil, errors.New("voice: settings service is not bound")
	}
	name := s.settingsString(KeyName)
	if name == "" {
		return nil, nil
	}
	voice, ok := s.Get(name)
	if !ok {
		return s.fallbackToDefault(ctx, name)
	}
	if voice.Builtin {
		// A built-in selected by an older revision remains usable: the TTS
		// image carries the matching embedding for its own revision.
		return nil, nil
	}
	if voice.Ready() {
		return nil, nil
	}
	return s.fallbackToDefault(ctx, name)
}

// fallbackToDefault writes the default voice as an override and returns the
// transition for the one announcement.
func (s *Service) fallbackToDefault(ctx context.Context, from string) (*Fallback, error) {
	to := s.defaultName
	if _, err := s.settings.Set(ctx, KeyName, to); err != nil {
		return nil, err
	}
	s.logger.Warn("voice: active voice is unusable, fell back to the default",
		"from", from, "to", to)
	return &Fallback{From: from, To: to}, nil
}

// settingsString reads the name key without requiring a settings service.
func (s *Service) settingsString(key string) string {
	if s.settings == nil {
		return ""
	}
	return strings.TrimSpace(s.settings.String(key))
}

// Definitions builds the two live settings shared with the Dashboard. Name is
// a plain string because a stored value may name a file that has since been
// removed; startup still succeeds and EnsureActive falls back. Rate is clamped
// by its validator so a bad write cannot reach the resampler.
func (s *Service) Definitions(cfg DefinitionConfig) []settings.Definition {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = s.defaultName
	}
	rate := cfg.Rate
	if rate <= 0 {
		rate = DefaultRate
	}
	return []settings.Definition{
		settings.String(KeyName, s.defaultName,
			settings.WithConfig(name),
			settings.WithKind(settings.Live),
			settings.WithDescription("Active voice. Changes apply to the next reply and the cached Agent Filler is re-synthesized in the new voice."),
			settings.WithValidator(validateName),
		),
		settings.Float(KeyRate, DefaultRate,
			settings.WithConfig(rate),
			settings.WithKind(settings.Live),
			settings.WithDescription("Speech-rate multiplier: 1.0 is normal, 1.2 is 20% faster. Implemented by resampling, so pitch rises with speed."),
			settings.WithValidator(validateRate),
		),
	}
}

// DefinitionConfig is the config.yaml layer for the two voice settings.
type DefinitionConfig struct {
	Name string
	Rate float64
}

func validateName(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("want a string, got %T", value)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("must not be blank")
	}
	return nil
}

func validateRate(value any) error {
	rate, ok := value.(float64)
	if !ok {
		return fmt.Errorf("want a number, got %T", value)
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < MinRate || rate > MaxRate {
		return fmt.Errorf("must be between %.1f and %.1f", MinRate, MaxRate)
	}
	return nil
}

// validRate keeps a hostile stored value from reaching the TTS request.
func validRate(rate float64) float64 {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < MinRate || rate > MaxRate {
		return DefaultRate
	}
	return roundRate(rate)
}

func clampRate(rate float64) float64 {
	if math.IsNaN(rate) || rate < MinRate {
		return MinRate
	}
	if rate > MaxRate {
		return MaxRate
	}
	return rate
}

// roundRate keeps repeated 0.1 steps from accumulating float noise.
func roundRate(rate float64) float64 {
	return math.Round(rate*100) / 100
}

// reload replaces the in-memory custom index from SQLite.
func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, file, size, mod_time, added_at, validated_revision, validated_at, stale, last_error, note
		 FROM voices ORDER BY name`)
	if err != nil {
		return fmt.Errorf("voice: read library: %w", err)
	}
	defer rows.Close()

	indexed := make(map[string]Voice)
	for rows.Next() {
		var (
			voice       Voice
			added       int64
			validatedAt string
		)
		if err := rows.Scan(&voice.Name, &voice.File, &voice.Size, &voice.modTime, &added,
			&voice.ValidatedRevision, &validatedAt, &voice.Stale, &voice.LastError, &voice.Note); err != nil {
			return fmt.Errorf("voice: read library row: %w", err)
		}
		voice.Path = filepath.Join(s.dir, filepath.FromSlash(voice.File))
		voice.AddedAt = time.Unix(0, added)
		if parsed, err := time.Parse(time.RFC3339, validatedAt); err == nil {
			voice.ValidatedAt = parsed
		}
		indexed[voice.Name] = voice
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("voice: read library: %w", err)
	}
	s.mu.Lock()
	s.voices = indexed
	s.mu.Unlock()
	return nil
}
