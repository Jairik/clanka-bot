// Package settings resolves stored overrides, config.yaml values and code
// defaults into one effective value (ADR-0003). Every definition reports the
// layer that won, and every override can be reset. Reads are served from an
// in-memory snapshot guarded by an RWMutex; writes update SQLite first, then
// the snapshot, so a change applies to the next request without a restart.
package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// ErrUnknownSetting reports a key that has no Definition.
var ErrUnknownSetting = errors.New("settings: unknown setting")

// Source is the layer an effective value came from.
type Source string

const (
	// SourceDefault is the code default.
	SourceDefault Source = "default"
	// SourceConfig is a value from config.yaml.
	SourceConfig Source = "config"
	// SourceOverride is a stored value set through the API or voice.
	SourceOverride Source = "override"
)

// Kind says when a changed value takes effect.
type Kind string

const (
	// Live settings are read per request and apply on the next one.
	Live Kind = "live"
	// RestartRequired settings name connections or devices that are only read
	// at startup.
	RestartRequired Kind = "restart_required"
)

// valueKind is the typed shape of a definition. It keeps the JSON layer
// honest: a bool override cannot be stored for a string setting.
type valueKind uint8

const (
	kindBool valueKind = iota + 1
	kindString
	kindInt
	kindFloat
	kindStrings
	kindDuration
)

// Definition describes one overridable setting. Build it with the typed
// constructors (Bool, String, Int, Strings, Duration) and Options.
type Definition struct {
	Key         string
	Default     any
	Kind        Kind
	Description string

	kind      valueKind
	config    any
	hasConfig bool
	validate  func(any) error
}

// Option adjusts a Definition.
type Option func(*Definition)

// WithConfig sets the config.yaml layer. A definition without it falls from
// the override straight to the code default.
func WithConfig(value any) Option {
	return func(d *Definition) {
		d.config = value
		d.hasConfig = true
	}
}

// WithKind marks when a change takes effect. The default is Live.
func WithKind(kind Kind) Option {
	return func(d *Definition) { d.Kind = kind }
}

// WithValidator adds a check applied to the config layer and every write.
// Defaults are trusted because they are code.
func WithValidator(validate func(any) error) Option {
	return func(d *Definition) { d.validate = validate }
}

// WithDescription adds a human-readable note for the API.
func WithDescription(text string) Option {
	return func(d *Definition) { d.Description = text }
}

// Bool defines a boolean setting.
func Bool(key string, def bool, opts ...Option) Definition {
	return newDefinition(key, kindBool, def, opts)
}

// String defines a string setting.
func String(key string, def string, opts ...Option) Definition {
	return newDefinition(key, kindString, def, opts)
}

// Int defines an integer setting.
func Int(key string, def int, opts ...Option) Definition {
	return newDefinition(key, kindInt, def, opts)
}

// Float defines a numeric setting such as a similarity threshold. Values cross
// the API as JSON numbers.
func Float(key string, def float64, opts ...Option) Definition {
	return newDefinition(key, kindFloat, def, opts)
}

// Strings defines a list-of-strings setting.
func Strings(key string, def []string, opts ...Option) Definition {
	return newDefinition(key, kindStrings, copyStrings(def), opts)
}

// Duration defines a duration setting. Values cross the API as Go duration
// strings such as "9m0s".
func Duration(key string, def time.Duration, opts ...Option) Definition {
	return newDefinition(key, kindDuration, def, opts)
}

func newDefinition(key string, kind valueKind, def any, opts []Option) Definition {
	d := Definition{Key: key, Default: def, Kind: Live, kind: kind}
	for _, opt := range opts {
		opt(&d)
	}
	return d
}

// Observer is notified after a change is committed and the in-memory snapshot
// is updated. keys are the settings that changed. It runs on the writing
// goroutine, so an implementation that does slow work must hand the keys to its
// own worker instead of blocking. One observer is enough for the live wording;
// this is not a general event bus.
type Observer func(keys []string)

// Value is one setting's effective value and every layer that contributes to
// it. Config and Override are nil when that layer is absent.
type Value struct {
	Key         string `json:"key"`
	Value       any    `json:"value"`
	Source      Source `json:"source"`
	Kind        Kind   `json:"kind"`
	Default     any    `json:"default"`
	Config      any    `json:"config"`
	Override    any    `json:"override"`
	Description string `json:"description,omitempty"`
}

// MarshalJSON reports durations as Go duration strings ("9m0s") rather than
// nanosecond numbers, matching the form SetJSON accepts.
func (v Value) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Key         string `json:"key"`
		Value       any    `json:"value"`
		Source      Source `json:"source"`
		Kind        Kind   `json:"kind"`
		Default     any    `json:"default"`
		Config      any    `json:"config"`
		Override    any    `json:"override"`
		Description string `json:"description,omitempty"`
	}{
		Key:         v.Key,
		Value:       jsonValue(v.Value),
		Source:      v.Source,
		Kind:        v.Kind,
		Default:     jsonValue(v.Default),
		Config:      jsonValue(v.Config),
		Override:    jsonValue(v.Override),
		Description: v.Description,
	})
}

func jsonValue(value any) any {
	if duration, ok := value.(time.Duration); ok {
		return duration.String()
	}
	return value
}

// Service resolves settings over one database.
type Service struct {
	db *sql.DB

	// writeMu serialises writers so SQLite order and the snapshot order match.
	writeMu sync.Mutex

	mu        sync.RWMutex
	defs      map[string]Definition
	order     []string
	overrides map[string]any
	observer  Observer
}

// New validates the definitions, reads stored overrides and returns the
// service. st must come from store.Open, which has already run migrations.
func New(ctx context.Context, st *store.Store, defs ...Definition) (*Service, error) {
	if st == nil || st.DB() == nil {
		return nil, fmt.Errorf("settings: store is required")
	}
	s := &Service{
		db:        st.DB(),
		defs:      make(map[string]Definition, len(defs)),
		overrides: make(map[string]any, len(defs)),
	}
	for _, def := range defs {
		if strings.TrimSpace(def.Key) == "" {
			return nil, fmt.Errorf("settings: definition key is required")
		}
		if _, exists := s.defs[def.Key]; exists {
			return nil, fmt.Errorf("settings: duplicate definition %q", def.Key)
		}
		if def.Kind != Live && def.Kind != RestartRequired {
			return nil, fmt.Errorf("settings: %s has unknown kind %q", def.Key, def.Kind)
		}
		if _, err := def.kind.encode(def.Default); err != nil {
			return nil, fmt.Errorf("settings: %s default: %w", def.Key, err)
		}
		if def.hasConfig {
			if err := def.check(def.config); err != nil {
				return nil, fmt.Errorf("settings: %s config: %w", def.Key, err)
			}
		}
		s.defs[def.Key] = def
		s.order = append(s.order, def.Key)
	}
	sort.Strings(s.order)

	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: read overrides: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("settings: read override: %w", err)
		}
		def, ok := s.defs[key]
		if !ok {
			// A removed setting leaves an orphan row. It is ignored rather
			// than deleting data a later version might understand.
			continue
		}
		value, err := def.kind.decode(json.RawMessage(raw))
		if err != nil {
			return nil, fmt.Errorf("settings: stored override %s: %w", key, err)
		}
		if err := def.validateValue(value); err != nil {
			return nil, fmt.Errorf("settings: stored override %s: %w", key, err)
		}
		s.overrides[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: read overrides: %w", err)
	}
	return s, nil
}

// SetObserver installs the one change observer, or clears it with nil. The
// observer is called after a successful write, never for a rejected value, so
// a failed request cannot leak into live behaviour.
func (s *Service) SetObserver(observer Observer) {
	s.mu.Lock()
	s.observer = observer
	s.mu.Unlock()
}

// notify calls the observer outside the lock. The caller has already updated
// the snapshot, so the observer can read the new effective values.
func (s *Service) notify(keys []string) {
	if len(keys) == 0 {
		return
	}
	s.mu.RLock()
	observer := s.observer
	s.mu.RUnlock()
	if observer == nil {
		return
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	observer(sorted)
}

// Get returns one effective value.
func (s *Service) Get(key string) (Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.defs[key]
	if !ok {
		return Value{}, false
	}
	return s.valueLocked(def), true
}

// GetMany returns several effective values from one consistent snapshot.
// A caller that needs related settings as a pair (a Tool's enabled flag and
// Permission) uses this so a concurrent batch write cannot mix layers.
func (s *Service) GetMany(keys ...string) map[string]Value {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make(map[string]Value, len(keys))
	for _, key := range keys {
		if def, ok := s.defs[key]; ok {
			values[key] = s.valueLocked(def)
		}
	}
	return values
}

// List returns every setting in key order.
func (s *Service) List() []Value {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]Value, 0, len(s.order))
	for _, key := range s.order {
		values = append(values, s.valueLocked(s.defs[key]))
	}
	return values
}

// String returns the effective string, or "" for an unknown or mistyped key.
func (s *Service) String(key string) string {
	value, _ := s.Get(key)
	text, _ := value.Value.(string)
	return text
}

// Bool returns the effective boolean, or false for an unknown or mistyped key.
func (s *Service) Bool(key string) bool {
	value, _ := s.Get(key)
	flag, _ := value.Value.(bool)
	return flag
}

// Int returns the effective integer, or 0 for an unknown or mistyped key.
func (s *Service) Int(key string) int {
	value, _ := s.Get(key)
	number, _ := value.Value.(int)
	return number
}

// Duration returns the effective duration, or 0 for an unknown or mistyped key.
func (s *Service) Duration(key string) time.Duration {
	value, _ := s.Get(key)
	duration, _ := value.Value.(time.Duration)
	return duration
}

// Float returns the effective number, or 0 for an unknown or mistyped key.
func (s *Service) Float(key string) float64 {
	value, _ := s.Get(key)
	number, _ := value.Value.(float64)
	return number
}

// Strings returns a copy of the effective list, or nil for an unknown or
// mistyped key.
func (s *Service) Strings(key string) []string {
	value, _ := s.Get(key)
	list, _ := value.Value.([]string)
	return copyStrings(list)
}

// Set stores one override and returns the new effective value.
func (s *Service) Set(ctx context.Context, key string, value any) (Value, error) {
	if err := s.SetMany(ctx, map[string]any{key: value}); err != nil {
		return Value{}, err
	}
	effective, _ := s.Get(key)
	return effective, nil
}

// SetJSON decodes one override from the API's JSON representation, validates
// it, and stores it.
func (s *Service) SetJSON(ctx context.Context, key string, raw json.RawMessage) (Value, error) {
	s.mu.RLock()
	def, ok := s.defs[key]
	s.mu.RUnlock()
	if !ok {
		return Value{}, fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	value, err := def.kind.decode(raw)
	if err != nil {
		return Value{}, fmt.Errorf("settings: %s: %w", key, err)
	}
	return s.Set(ctx, key, value)
}

// SetMany validates every value, writes them in one transaction, then swaps
// the in-memory snapshot. A batch is all or nothing.
func (s *Service) SetMany(ctx context.Context, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	type write struct {
		key   string
		raw   string
		value any
	}
	prepared := make([]write, 0, len(values))
	for key, value := range values {
		s.mu.RLock()
		def, ok := s.defs[key]
		s.mu.RUnlock()
		if !ok {
			return fmt.Errorf("%w %q", ErrUnknownSetting, key)
		}
		if err := def.check(value); err != nil {
			return fmt.Errorf("settings: %s: %w", key, err)
		}
		raw, err := def.kind.encode(value)
		if err != nil {
			return fmt.Errorf("settings: %s: %w", key, err)
		}
		// Keep the canonical decode, so a caller mutating its slice after Set
		// cannot change the cached value.
		canonical, err := def.kind.decode(raw)
		if err != nil {
			return fmt.Errorf("settings: %s: %w", key, err)
		}
		prepared = append(prepared, write{key: key, raw: string(raw), value: canonical})
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: begin write: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, w := range prepared {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			w.key, w.raw, now); err != nil {
			tx.Rollback()
			return fmt.Errorf("settings: write %s: %w", w.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: commit write: %w", err)
	}

	// The database is the source of truth; the snapshot changes only after a
	// successful commit, so a failed write cannot leak into a request.
	keys := make([]string, 0, len(prepared))
	s.mu.Lock()
	for _, w := range prepared {
		s.overrides[w.key] = w.value
		keys = append(keys, w.key)
	}
	s.mu.Unlock()
	s.notify(keys)
	return nil
}

// Reset deletes one override and returns the value the lower layers apply.
func (s *Service) Reset(ctx context.Context, key string) (Value, error) {
	if err := s.ResetMany(ctx, key); err != nil {
		return Value{}, err
	}
	effective, _ := s.Get(key)
	return effective, nil
}

// ResetMany deletes several overrides in one transaction. Resetting a setting
// without an override is a no-op.
func (s *Service) ResetMany(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	unique := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		s.mu.RLock()
		_, ok := s.defs[key]
		s.mu.RUnlock()
		if !ok {
			return fmt.Errorf("%w %q", ErrUnknownSetting, key)
		}
		if !seen[key] {
			seen[key] = true
			unique = append(unique, key)
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: begin reset: %w", err)
	}
	for _, key := range unique {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			tx.Rollback()
			return fmt.Errorf("settings: reset %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: commit reset: %w", err)
	}

	s.mu.Lock()
	for _, key := range unique {
		delete(s.overrides, key)
	}
	s.mu.Unlock()
	s.notify(unique)
	return nil
}

// valueLocked resolves the three layers for one definition. The caller holds
// at least the read lock.
func (s *Service) valueLocked(def Definition) Value {
	value := Value{
		Key:         def.Key,
		Kind:        def.Kind,
		Default:     copyValue(def.Default),
		Description: def.Description,
		Source:      SourceDefault,
		Value:       copyValue(def.Default),
	}
	if def.hasConfig {
		value.Config = copyValue(def.config)
		value.Value = copyValue(def.config)
		value.Source = SourceConfig
	}
	if override, ok := s.overrides[def.Key]; ok {
		value.Override = copyValue(override)
		value.Value = copyValue(override)
		value.Source = SourceOverride
	}
	return value
}

// copyValue copies the slice layers so a caller cannot mutate the snapshot.
func copyValue(value any) any {
	if list, ok := value.([]string); ok {
		return copyStrings(list)
	}
	return value
}

// copyStrings copies a string list and keeps an empty list distinct from a nil
// one, so an empty override reports [] rather than null.
func copyStrings(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	copy(out, list)
	return out
}

// validateValue applies the definition's optional validator.
func (d Definition) validateValue(value any) error {
	if d.validate == nil {
		return nil
	}
	return d.validate(value)
}

// check type-checks a value for this definition, then runs its validator.
func (d Definition) check(value any) error {
	if _, err := d.kind.encode(value); err != nil {
		return err
	}
	return d.validateValue(value)
}

// decode turns a stored or API JSON value into the definition's Go type.
func (k valueKind) decode(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("value is required")
	}
	switch k {
	case kindBool:
		var value bool
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("want a boolean: %w", err)
		}
		return value, nil
	case kindString:
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("want a string: %w", err)
		}
		return value, nil
	case kindInt:
		var value int
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("want an integer: %w", err)
		}
		return value, nil
	case kindFloat:
		var value float64
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("want a number: %w", err)
		}
		return value, nil
	case kindStrings:
		var value []string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("want a list of strings: %w", err)
		}
		return copyStrings(value), nil
	case kindDuration:
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("want a duration string: %w", err)
		}
		value, err := time.ParseDuration(text)
		if err != nil {
			return nil, fmt.Errorf("want a duration such as 9m0s: %w", err)
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unknown setting kind")
	}
}

// encode type-checks a Go value and returns its canonical JSON form.
func (k valueKind) encode(value any) (json.RawMessage, error) {
	switch k {
	case kindBool:
		typed, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("want a boolean, got %T", value)
		}
		return json.Marshal(typed)
	case kindString:
		typed, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("want a string, got %T", value)
		}
		return json.Marshal(typed)
	case kindInt:
		typed, ok := value.(int)
		if !ok {
			return nil, fmt.Errorf("want an integer, got %T", value)
		}
		return json.Marshal(typed)
	case kindFloat:
		typed, ok := value.(float64)
		if !ok {
			return nil, fmt.Errorf("want a number, got %T", value)
		}
		return json.Marshal(typed)
	case kindStrings:
		typed, ok := value.([]string)
		if !ok {
			return nil, fmt.Errorf("want a list of strings, got %T", value)
		}
		return json.Marshal(typed)
	case kindDuration:
		typed, ok := value.(time.Duration)
		if !ok {
			return nil, fmt.Errorf("want a duration, got %T", value)
		}
		return json.Marshal(typed.String())
	default:
		return nil, fmt.Errorf("unknown setting kind")
	}
}
