package sound

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// Rescan indexes the built-in chime and every valid .wav in sounds/, updates
// names and durations, and drops rows for files that have disappeared. An
// invalid file is logged and skipped: one bad drop must not stop startup. The
// operation is idempotent and keeps each name's original added_at.
func (s *Service) Rescan(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sound: library is not open")
	}
	entries, err := s.scanFiles()
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sound: begin index: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	// Built-ins first so a volume file cannot shadow the shipped chime.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sounds (name, path, builtin, duration_ms, added_at) VALUES (?, '', 1, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET path = '', builtin = 1, duration_ms = excluded.duration_ms`,
		s.chime.Name, durationMillis(s.chime.Duration), now); err != nil {
		return fmt.Errorf("sound: index builtin %s: %w", s.chime.Name, err)
	}

	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Name] = true
		relative, err := filepath.Rel(s.libraryDir, entry.Path)
		if err != nil {
			return fmt.Errorf("sound: index %s: %w", entry.Path, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sounds (name, path, builtin, duration_ms, added_at) VALUES (?, ?, 0, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET path = excluded.path, builtin = 0, duration_ms = excluded.duration_ms`,
			entry.Name, filepath.ToSlash(relative), durationMillis(entry.Duration), now); err != nil {
			return fmt.Errorf("sound: index %s: %w", entry.Name, err)
		}
	}

	// Remove library rows whose file vanished. The builtin row never matches.
	rows, err := tx.QueryContext(ctx, `SELECT name, path FROM sounds WHERE builtin = 0`)
	if err != nil {
		return fmt.Errorf("sound: read index: %w", err)
	}
	var stale []string
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			rows.Close()
			return fmt.Errorf("sound: read index: %w", err)
		}
		if !present[name] {
			stale = append(stale, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sound: read index: %w", err)
	}
	rows.Close()
	for _, name := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sounds WHERE name = ?`, name); err != nil {
			return fmt.Errorf("sound: drop stale %s: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sound: commit index: %w", err)
	}
	return s.reload(ctx)
}

// scanFiles walks the small, flat sounds/ folder. It is not recursive: the
// Dashboard and a human copy files there directly.
func (s *Service) scanFiles() ([]Sound, error) {
	entries, err := os.ReadDir(s.libraryDir)
	if err != nil {
		return nil, fmt.Errorf("sound: read library %s: %w", s.libraryDir, err)
	}
	out := make([]Sound, 0, len(entries))
	seen := map[string]bool{BuiltinChime: true}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".wav") {
			continue
		}
		path := filepath.Join(s.libraryDir, entry.Name())
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			s.logger.Warn("sound: skip unreadable library file", "file", entry.Name(), "error", err.Error())
			continue
		}
		if !within(s.libraryDir, real) {
			s.logger.Warn("sound: skip library file outside sounds/", "file", entry.Name())
			continue
		}
		info, err := audio.InspectWAV(real)
		if err != nil {
			s.logger.Warn("sound: skip invalid library file", "file", entry.Name(), "error", err.Error())
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if name == "" || seen[strings.ToLower(name)] {
			s.logger.Warn("sound: skip duplicate library name", "file", entry.Name())
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, Sound{
			Name:     name,
			Path:     real,
			Duration: info.Duration,
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// durationMillis rounds a duration up to the index's millisecond column. The
// rounding is deliberate: a file just over a role cap must not slip through
// assignment because truncation made it look shorter.
func durationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// reload replaces the in-memory index from the database, preserving added_at.
func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, path, builtin, duration_ms, added_at FROM sounds ORDER BY name`)
	if err != nil {
		return fmt.Errorf("sound: read library: %w", err)
	}
	defer rows.Close()

	indexed := make(map[string]Sound)
	for rows.Next() {
		var (
			name       string
			path       string
			builtin    int
			durationMS int64
			added      string
		)
		if err := rows.Scan(&name, &path, &builtin, &durationMS, &added); err != nil {
			return fmt.Errorf("sound: read library row: %w", err)
		}
		entry := Sound{
			Name:     name,
			Builtin:  builtin != 0,
			Duration: time.Duration(durationMS) * time.Millisecond,
		}
		if !entry.Builtin && path != "" {
			entry.Path = filepath.Join(s.libraryDir, filepath.FromSlash(path))
		}
		if parsed, err := time.Parse(time.RFC3339, added); err == nil {
			entry.AddedAt = parsed
		}
		indexed[name] = entry
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sound: read library: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the builtin's authoritative PCM and name but preserve the indexed
	// first-added time, so a restart does not look like a new sound.
	if stored, ok := indexed[BuiltinChime]; ok {
		builtin := s.chime
		builtin.AddedAt = stored.AddedAt
		indexed[BuiltinChime] = builtin
	}
	s.sounds = indexed
	return nil
}
