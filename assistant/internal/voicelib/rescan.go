package voicelib

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rescan indexes the .safetensors files in the library folder, validates new or
// changed files with the TTS smoke probe, and records the model revision each
// one passed against. An unchanged file validated against a different running
// revision is marked stale instead of being smoke-tested again: the spec's
// stale rule is an explicit signal to re-export on the desktop, not a silent
// revalidation. A failed or unavailable probe leaves a new file unvalidated
// (selectable only after a later successful rescan), never unusable-because-
// startup-failed: the caller falls back and continues.
//
// Header validation is always local and always runs before a file can be
// indexed. Only .safetensors is considered; .pt/.pth/.bin are rejected by the
// safetensors check even if someone renames them.
func (s *Service) Rescan(ctx context.Context, probe Probe) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("voice: library is not open")
	}
	revision := s.Revision()
	canValidate := false
	if probe != nil {
		current, err := probe.Revision(ctx)
		if err != nil {
			// Do not turn a TTS restart into stale markers. A later rescan with
			// a live service records the new revision.
			s.logger.Warn("voice: tts revision unavailable, keeping the indexed revision", "error", err.Error())
		} else {
			revision = strings.TrimSpace(current)
			canValidate = true
			s.mu.Lock()
			s.revision = revision
			s.mu.Unlock()
		}
	}

	old := s.snapshotVoices()
	scanned, err := s.scanFiles(time.Now().UTC())
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for i := range scanned {
		entry := &scanned[i]
		previous, had := old[entry.voice.Name]
		entry.changed = !had || previous.Size != entry.voice.Size || previous.modTime != entry.voice.modTime

		switch {
		case had && !entry.changed && previous.ValidatedRevision != "":
			// An unchanged file already passed a smoke test. If the running
			// model moved, keep the evidence but mark it stale.
			entry.voice.ValidatedRevision = previous.ValidatedRevision
			entry.voice.ValidatedAt = previous.ValidatedAt
			entry.voice.AddedAt = previous.AddedAt
			entry.voice.Note = previous.Note
			// An unavailable revision must not silently clear a stale marker:
			// only a live TTS answer can prove the state is current. When TTS
			// rolls back to the recorded revision, the state is valid again.
			if revision == "" {
				entry.voice.Stale = previous.Stale
			} else {
				entry.voice.Stale = previous.ValidatedRevision != revision
			}
			entry.voice.LastError = ""
		case canValidate:
			remote := path.Join(s.ttsDir, entry.voice.File)
			validatedRevision, err := probe.Validate(ctx, remote)
			if err != nil {
				// Keep the row so the Dashboard can show the failure and #64 can
				// report it, but never let voice.set select it.
				entry.voice.ValidatedRevision = ""
				entry.voice.LastError = err.Error()
				entry.voice.Stale = false
				s.logger.Warn("voice: model smoke test failed", "voice", entry.voice.Name, "error", err.Error())
			} else {
				validatedRevision = strings.TrimSpace(validatedRevision)
				if validatedRevision == "" {
					validatedRevision = revision
				}
				entry.voice.ValidatedRevision = validatedRevision
				entry.voice.ValidatedAt = now
				entry.voice.LastError = ""
				entry.voice.Stale = revision != "" && validatedRevision != revision
			}
		default:
			// Header-only validation: the file is indexed but cannot be chosen
			// until a rescan with a live TTS service performs the smoke test.
			entry.voice.ValidatedRevision = ""
			entry.voice.LastError = "waiting for a TTS model smoke test"
			entry.voice.Stale = false
		}
		if had {
			// added_at is first-index time, not the file's mtime.
			entry.voice.AddedAt = previous.AddedAt
			entry.voice.Note = previous.Note
		}
	}

	if err := s.writeIndex(ctx, scanned, old); err != nil {
		return err
	}
	return s.reload(ctx)
}

// scanResult is one header-valid file plus the change decision made against the
// previous row.
type scanResult struct {
	voice   Voice
	changed bool
}

// snapshotVoices copies the custom index so the scan decisions cannot race a
// concurrent reload or Tool read.
func (s *Service) snapshotVoices() map[string]Voice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Voice, len(s.voices))
	for name, voice := range s.voices {
		out[name] = voice
	}
	return out
}

// scanFiles walks the flat voices/ folder. It is not recursive: a human or the
// #64 upload path copies one file at a time into this directory.
func (s *Service) scanFiles(now time.Time) ([]scanResult, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("voice: read library %s: %w", s.dir, err)
	}
	out := make([]scanResult, 0, len(entries))
	// A custom file may not shadow a built-in voice: the built-in is what the
	// TTS image warmed, and the same name would make selection ambiguous.
	seen := make(map[string]bool, len(entries)+len(s.builtins))
	for _, name := range s.builtins {
		seen[strings.ToLower(name)] = true
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Upload staging (.upload-*.safetensors) and replacement backups
		// (.replace-*.bak) are hidden so a scan can never index a file that is
		// about to be renamed or removed.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".safetensors" {
			// A .pt/.pth/.bin state is a pickle-backed torch file. The voice
			// library never loads one; say so once so a mistaken copy is visible.
			if extension == ".pt" || extension == ".pth" || extension == ".bin" || extension == ".ckpt" {
				s.logger.Warn("voice: skip unsupported voice state format", "file", entry.Name(), "want", ".safetensors")
			}
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if name == "" || seen[strings.ToLower(name)] {
			s.logger.Warn("voice: skip duplicate or empty voice name", "file", entry.Name())
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			s.logger.Warn("voice: skip unreadable library file", "file", entry.Name(), "error", err.Error())
			continue
		}
		if !within(s.dir, real) {
			s.logger.Warn("voice: skip library file outside voices/", "file", entry.Name())
			continue
		}
		info, err := os.Stat(real)
		if err != nil {
			s.logger.Warn("voice: skip unstattable library file", "file", entry.Name(), "error", err.Error())
			continue
		}
		if info.Size() > MaxStateBytes {
			s.logger.Warn("voice: skip oversized voice state", "file", entry.Name(), "bytes", info.Size(), "limit", MaxStateBytes)
			continue
		}
		if _, err := InspectState(real); err != nil {
			s.logger.Warn("voice: skip invalid voice state", "file", entry.Name(), "error", err.Error())
			continue
		}
		relative, err := filepath.Rel(s.dir, real)
		if err != nil {
			s.logger.Warn("voice: skip library file", "file", entry.Name(), "error", err.Error())
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, scanResult{voice: Voice{
			Name:    name,
			File:    filepath.ToSlash(relative),
			Path:    real,
			Size:    info.Size(),
			AddedAt: now,
			modTime: info.ModTime().UnixNano(),
		}})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].voice.Name) < strings.ToLower(out[j].voice.Name) })
	return out, nil
}

// writeIndex replaces the voices table in one transaction. Deleted files take
// their rows with them; a header-invalid replacement is treated as removed
// until the file is fixed and rescanned.
func (s *Service) writeIndex(ctx context.Context, scanned []scanResult, old map[string]Voice) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("voice: begin index: %w", err)
	}
	defer tx.Rollback()

	present := make(map[string]bool, len(scanned))
	for _, entry := range scanned {
		voice := entry.voice
		present[voice.Name] = true
		validatedAt := ""
		if !voice.ValidatedAt.IsZero() {
			validatedAt = voice.ValidatedAt.UTC().Format(time.RFC3339)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO voices (name, file, size, mod_time, added_at, validated_revision, validated_at, stale, last_error, note)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET
			     file = excluded.file,
			     size = excluded.size,
			     mod_time = excluded.mod_time,
			     validated_revision = excluded.validated_revision,
			     validated_at = excluded.validated_at,
			     stale = excluded.stale,
			     last_error = excluded.last_error,
			     note = excluded.note`,
			voice.Name, voice.File, voice.Size, voice.modTime, voice.AddedAt.UnixNano(),
			voice.ValidatedRevision, validatedAt, voice.Stale, voice.LastError, voice.Note); err != nil {
			return fmt.Errorf("voice: index %s: %w", voice.Name, err)
		}
	}
	for name := range old {
		if present[name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM voices WHERE name = ?`, name); err != nil {
			return fmt.Errorf("voice: drop stale %s: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("voice: commit index: %w", err)
	}
	return nil
}

// within reports whether target is inside dir after symlink resolution.
func within(dir, target string) bool {
	relative, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// SetNote records the optional free-text source/consent note. It is recorded
// only: nothing enforces or validates the claim. #64's upload page calls it.
func (s *Service) SetNote(ctx context.Context, name, note string) error {
	voice, ok := s.Get(name)
	if !ok || voice.Builtin {
		return fmt.Errorf("%w %q", ErrUnknownVoice, name)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE voices SET note = ? WHERE name = ?`, strings.TrimSpace(note), voice.Name); err != nil {
		return fmt.Errorf("voice: set note %s: %w", name, err)
	}
	return s.reload(ctx)
}
