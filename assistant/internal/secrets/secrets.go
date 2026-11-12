// Package secrets owns the write-only secrets table (ADR-0003). A secret is
// resolved from the stored value first and the environment bootstrap second;
// the stored value wins. Values are never logged and never become a settings
// definition, so the settings API cannot list or return them. Only metadata
// (set, last four, updated date and the active source) crosses the API.
package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrUnknownSecret reports a name no module declared.
var ErrUnknownSecret = errors.New("secrets: unknown secret")

// ErrEmptyValue reports a write with no usable value.
var ErrEmptyValue = errors.New("secrets: value is required")

// Source is the layer the active secret came from.
type Source string

const (
	// SourceNone means neither the store nor the environment has a value.
	SourceNone Source = "none"
	// SourceEnv is the .env bootstrap.
	SourceEnv Source = "env"
	// SourceStore is the stored value, which wins over the environment.
	SourceStore Source = "store"
)

// minLastFourLength hides the suffix for short values: four characters of a
// four- or eight-character secret would reveal all or half of it.
const minLastFourLength = 12

// Metadata is the only shape a secret takes outside this package and the
// model client. It never carries the value.
type Metadata struct {
	Name      string     `json:"name"`
	Set       bool       `json:"set"`
	Source    Source     `json:"source"`
	Last4     string     `json:"last4"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// stored is one cached row. The cache is updated only after a successful
// write, so a failed database call never changes what the next request sees.
type stored struct {
	value     string
	updatedAt time.Time
}

// Service resolves and writes secrets over the assistant's one database.
// Declared names come from Tool module metadata and the Agent provider; the
// API can only set or clear those names.
type Service struct {
	db          *sql.DB
	declared    []string
	declaredSet map[string]struct{}

	mu     sync.RWMutex
	stored map[string]stored
}

// New loads the declared names and the stored rows. Duplicate declarations are
// collapsed; an empty declaration is a wiring error. Unknown rows left by a
// removed module are kept for a later declaration rather than deleted.
func New(ctx context.Context, db *sql.DB, declared []string) (*Service, error) {
	if db == nil {
		return nil, errors.New("secrets: database is required")
	}
	svc := &Service{
		db:          db,
		declaredSet: make(map[string]struct{}, len(declared)),
		stored:      make(map[string]stored),
	}
	for _, raw := range declared {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, errors.New("secrets: declared secret name is empty")
		}
		if _, ok := svc.declaredSet[name]; ok {
			continue
		}
		svc.declaredSet[name] = struct{}{}
		svc.declared = append(svc.declared, name)
	}

	rows, err := db.QueryContext(ctx, `SELECT name, value, updated_at FROM secrets`)
	if err != nil {
		return nil, fmt.Errorf("secrets: read stored secrets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, value, updated string
		if err := rows.Scan(&name, &value, &updated); err != nil {
			return nil, fmt.Errorf("secrets: scan stored secret: %w", err)
		}
		at, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("secrets: stored timestamp for %s: %w", name, err)
		}
		svc.stored[name] = stored{value: value, updatedAt: at}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read stored secrets: %w", err)
	}
	return svc, nil
}

// Get resolves one secret for a module or the model client. The stored value
// wins over the environment; an empty environment value counts as absent.
func (s *Service) Get(name string) (string, bool) {
	s.mu.RLock()
	row, ok := s.stored[name]
	s.mu.RUnlock()
	if ok {
		return row.value, true
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

// Metadata reports one declared secret's safe summary.
func (s *Service) Metadata(name string) (Metadata, bool) {
	if !s.isDeclared(name) {
		return Metadata{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadataLocked(name), true
}

// List reports every declared secret, in declaration order, without values.
func (s *Service) List() []Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]Metadata, 0, len(s.declared))
	for _, name := range s.declared {
		list = append(list, s.metadataLocked(name))
	}
	return list
}

// Set stores one declared secret and returns its metadata. Only the stored
// layer changes; clearing it exposes the environment value again.
func (s *Service) Set(ctx context.Context, name, value string) (Metadata, error) {
	name = strings.TrimSpace(name)
	if !s.isDeclared(name) {
		return Metadata{}, ErrUnknownSecret
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return Metadata{}, ErrEmptyValue
	}
	at := time.Now().UTC()

	// The write lock spans the database call and the cache update so two
	// concurrent writers cannot leave the cache with the older row.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (name, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, value, at.Format(time.RFC3339Nano)); err != nil {
		return Metadata{}, fmt.Errorf("secrets: store %s: %w", name, err)
	}
	s.stored[name] = stored{value: value, updatedAt: at}
	return s.metadataLocked(name), nil
}

// Clear removes the stored value. The environment bootstrap, when present,
// becomes the active source again.
func (s *Service) Clear(ctx context.Context, name string) (Metadata, error) {
	name = strings.TrimSpace(name)
	if !s.isDeclared(name) {
		return Metadata{}, ErrUnknownSecret
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name); err != nil {
		return Metadata{}, fmt.Errorf("secrets: clear %s: %w", name, err)
	}
	delete(s.stored, name)
	return s.metadataLocked(name), nil
}

// isDeclared reads the immutable declaration set built by New.
func (s *Service) isDeclared(name string) bool {
	_, ok := s.declaredSet[name]
	return ok
}

// metadataLocked builds one summary. The caller holds s.mu.
func (s *Service) metadataLocked(name string) Metadata {
	meta := Metadata{Name: name, Source: SourceNone}
	if row, ok := s.stored[name]; ok {
		at := row.updatedAt
		return Metadata{
			Name:      name,
			Set:       true,
			Source:    SourceStore,
			Last4:     lastFour(row.value),
			UpdatedAt: &at,
		}
	}
	if value, ok := os.LookupEnv(name); ok && value != "" {
		meta.Set = true
		meta.Source = SourceEnv
		meta.Last4 = lastFour(value)
	}
	return meta
}

// lastFour returns a suffix only when the value is long enough that the
// suffix is a hint rather than a large share of the secret.
func lastFour(value string) string {
	if len(value) < minLastFourLength {
		return ""
	}
	return value[len(value)-4:]
}
