package sound

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNotFound reports a delete of a name that is not in the library.
var ErrNotFound = errors.New("sound: not in the sound library")

// Delete removes one uploaded or dropped library sound: its WAV file and its
// index row. The built-in chime cannot be deleted, and neither can a sound a
// Cue role currently resolves to, so a role never goes silent by surprise.
// A per-Alarm override naming the sound falls back to sounds.alarm_ring.
func (s *Service) Delete(ctx context.Context, name string) error {
	if s == nil || s.db == nil {
		return errors.New("sound: library is not open")
	}
	// Serialise with Import so a delete cannot race a publish of the same name.
	s.importMu.Lock()
	defer s.importMu.Unlock()

	entry, ok := s.Find(name)
	if !ok {
		return ErrNotFound
	}
	if entry.Builtin || entry.Path == "" {
		return fmt.Errorf("sound: the built-in %s cannot be deleted", entry.Name)
	}
	if roles := s.rolesUsing(entry.Name); len(roles) > 0 {
		return fmt.Errorf("sound: %s is assigned to the %s Cue; choose another sound there first",
			entry.Name, strings.Join(roles, ", "))
	}
	if !within(s.libraryDir, entry.Path) {
		return fmt.Errorf("sound: refusing to delete outside %s", s.libraryDir)
	}
	if err := os.Remove(entry.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sound: delete %s: %w", entry.Name, err)
	}
	s.mu.Lock()
	delete(s.cache, entry.Path)
	s.mu.Unlock()
	// Rescan drops the index row for the vanished file.
	return s.Rescan(ctx)
}

// rolesUsing lists the Cue roles whose effective assignment resolves to name.
func (s *Service) rolesUsing(name string) []string {
	s.mu.RLock()
	settingsSvc := s.settings
	s.mu.RUnlock()
	if settingsSvc == nil {
		return nil
	}
	var roles []string
	for _, role := range Roles {
		entry, ok, err := s.Resolve(settingsSvc.String(Key(role)))
		if err == nil && ok && strings.EqualFold(entry.Name, name) {
			roles = append(roles, string(role))
		}
	}
	return roles
}
