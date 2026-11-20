package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetPrivate reads a non-user value from the settings table.
// These rows have no Definition, so List, Get, Set and the Settings page
// never show them. A missing key returns false.
func (s *Service) GetPrivate(ctx context.Context, key string) (string, bool, error) {
	if err := s.rejectUserKey(key); err != nil {
		return "", false, err
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("settings: read %s: %w", key, err)
	}
	var value string
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", false, fmt.Errorf("settings: read %s: %w", key, err)
	}
	return value, true, nil
}

// SetPrivate stores a non-user value in the settings table.
// A defined user setting is rejected so this path cannot skip its validator.
// The write does not update the user-setting snapshot and does not notify observers.
func (s *Service) SetPrivate(ctx context.Context, key, value string) error {
	if err := s.rejectUserKey(key); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("settings: write %s: %w", key, err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw), now); err != nil {
		return fmt.Errorf("settings: write %s: %w", key, err)
	}
	return nil
}

// rejectUserKey reports a blank key or one that is a user setting.
func (s *Service) rejectUserKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("settings: private key is required")
	}
	s.mu.RLock()
	_, user := s.defs[key]
	s.mu.RUnlock()
	if user {
		return fmt.Errorf("settings: %s is a user setting", key)
	}
	return nil
}
