package voicelib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Upload failures are separated so the Dashboard can answer with a short,
// user-facing message instead of leaking temp paths or model internals.
var (
	ErrUnsupportedState      = errors.New("voice: only .safetensors voice states are supported")
	ErrStateTooLarge         = errors.New("voice: voice state is larger than the 32 MB limit")
	ErrInvalidState          = errors.New("voice: invalid state")
	ErrVoiceExists           = errors.New("voice: a voice with that name already exists")
	ErrBuiltinName           = errors.New("voice: that name belongs to a built-in voice")
	ErrValidationUnavailable = errors.New("voice: the speech service is not configured for validation")
	ErrStagingUnsupported    = errors.New("voice: this speech service cannot validate a staging file without caching")
	ErrReplaceUnsupported    = errors.New("voice: this speech service cannot safely replace a cached voice state")
	ErrSmokeFailed           = errors.New("voice: model smoke test failed")
)

// StateExtension is the one accepted voice-state suffix.
const StateExtension = ".safetensors"

// maxStateNameRunes keeps filenames usable in a URL and on the volume without
// letting a client create an unbounded display name.
const maxStateNameRunes = 80

// MaxNoteRunes bounds the optional consent note accepted by the Dashboard.
const MaxNoteRunes = 2000

// EphemeralValidator is the Probe extension an import needs. The state is
// validated from a hidden staging file before it has a final name; a probe
// that cached that path would keep a dead temp entry forever. TTSProbe
// implements this by sending cache=false.
type EphemeralValidator interface {
	ValidateEphemeral(ctx context.Context, remotePath string) (revision string, err error)
}

// Forgetter releases a cached voice state by its path inside the TTS mount.
// It is required for replacement: without it the model would keep serving the
// old bytes cached under the same path.
type Forgetter interface {
	Forget(ctx context.Context, remotePath string) error
}

// ImportOptions controls one upload. Probe is required: a state is only
// published after a real model smoke test. Replace permits an explicit
// overwrite of an existing custom entry after the usual staging and rollback.
type ImportOptions struct {
	Probe   Probe
	Replace bool
}

// Import stages, validates and atomically publishes one uploaded voice state.
//
// The order is deliberate and fail-closed:
//  1. the filename must be .safetensors; the upload is copied into a hidden
//     temp file under the library so the rename stays atomic;
//  2. InspectState checks the bounded header before TTS is involved at all;
//  3. TTS loads the staged state without caching it, synthesizes its smoke
//     phrase, and returns the model revision it passed against;
//  4. the staged file is renamed into place (an existing file is first moved
//     aside), the final path's cache entry is dropped, and a rescan records
//     the revision; any failure restores the previous file and index.
func (s *Service) Import(ctx context.Context, src io.Reader, filename string, opts ImportOptions) (Voice, error) {
	if s == nil || s.db == nil {
		return Voice{}, errors.New("voice: library is not open")
	}
	if src == nil {
		return Voice{}, errors.New("voice: upload is empty")
	}
	if opts.Probe == nil {
		return Voice{}, ErrValidationUnavailable
	}
	if _, ok := opts.Probe.(EphemeralValidator); !ok {
		return Voice{}, ErrStagingUnsupported
	}

	name, err := StateName(filename)
	if err != nil {
		return Voice{}, err
	}
	if s.BuiltinName(name) {
		return Voice{}, fmt.Errorf("%w: %q", ErrBuiltinName, name)
	}
	target := filepath.Join(s.dir, name+StateExtension)
	if !within(s.dir, target) {
		return Voice{}, fmt.Errorf("voice: refusing to publish outside %s", s.dir)
	}

	// One import at a time: two uploads with the same name must not race their
	// backup/rename steps, and a rescan must never observe a half-published
	// file. The scan also ignores dot-prefixed staging files.
	s.importMu.Lock()
	defer s.importMu.Unlock()

	existing, err := existingState(target, name)
	if err != nil {
		return Voice{}, err
	}
	previous, hadPrevious := s.Get(name)
	if existing && !opts.Replace {
		return Voice{}, fmt.Errorf("%w: %q (tick Replace to overwrite it)", ErrVoiceExists, name)
	}
	if existing {
		if _, ok := opts.Probe.(Forgetter); !ok {
			return Voice{}, ErrReplaceUnsupported
		}
	}

	tempPath, err := s.stage(ctx, src)
	if err != nil {
		return Voice{}, err
	}
	// The temp file has a final .safetensors suffix so TTS sees a normal state;
	// the leading dot keeps a concurrent scan from indexing it.
	defer os.Remove(tempPath)

	if _, err := InspectState(tempPath); err != nil {
		return Voice{}, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}

	remoteStaged := path.Join(s.ttsDir, filepath.Base(tempPath))
	revision, err := opts.Probe.(EphemeralValidator).ValidateEphemeral(ctx, remoteStaged)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Voice{}, fmt.Errorf("%w: %v", ErrSmokeFailed, ctxErr)
		}
		return Voice{}, fmt.Errorf("%w: %v", ErrSmokeFailed, err)
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return Voice{}, fmt.Errorf("%w: no model revision was recorded", ErrSmokeFailed)
	}

	backup, err := publishState(tempPath, target)
	if err != nil {
		return Voice{}, err
	}
	// From here on the final path exists. Failures restore the previous bytes
	// and index row (or remove both for a new name), so the page never shows a
	// selectable state that failed.
	remoteFinal := path.Join(s.ttsDir, name+StateExtension)
	rollback := func(cause error) (Voice, error) {
		s.rollbackImport(target, backup, name, remoteFinal, previous, existing && hadPrevious, opts.Probe)
		return Voice{}, cause
	}

	if forget, ok := opts.Probe.(Forgetter); ok {
		// Drop any cached state for this path, including a previous export and
		// any stale entry left by a manual file replacement. Otherwise the
		// final-path validation could "validate" the old bytes.
		if err := forget.Forget(ctx, remoteFinal); err != nil {
			return rollback(fmt.Errorf("%w: could not clear the cached voice state: %v", ErrSmokeFailed, err))
		}
	}
	if err := s.Rescan(ctx, opts.Probe); err != nil {
		return rollback(fmt.Errorf("voice: index upload: %w", err))
	}
	voice, ok := s.Get(name)
	if !ok || !voice.Ready() {
		detail := ""
		if ok {
			detail = voice.LastError
		}
		return rollback(fmt.Errorf("%w: %s", ErrSmokeFailed, strings.TrimSpace(detail)))
	}
	if backup != "" {
		if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A hidden .bak file is harmless and never scanned; leaving it is
			// better than failing an already-published import.
			s.logger.Warn("voice: remove replacement backup", "path", backup, "error", err.Error())
		}
	}
	return voice, nil
}

// cleanupStaging removes hidden upload and replacement files left by a crash.
// It runs before any upload can exist, so an unconditional removal is safe;
// directories and every visible file are untouched.
func cleanupStaging(dir string, logger *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn("voice: read library for staging cleanup", "error", err.Error())
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(name, ".upload-") && !strings.HasPrefix(name, ".replace-") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			logger.Warn("voice: remove stale staging file", "file", name, "error", err.Error())
		}
	}
}

// StateName turns an upload's filename into a confined library name. Only the
// .safetensors suffix is accepted, the name may not start with a dot or contain
// a separator, and it is capped so it stays usable in a URL and on the volume.
func StateName(filename string) (string, error) {
	base := filepath.Base(strings.TrimSpace(filename))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("%w: the upload has no filename", ErrUnsupportedState)
	}
	extension := filepath.Ext(base)
	if !strings.EqualFold(extension, StateExtension) {
		lower := strings.ToLower(extension)
		if lower == ".pt" || lower == ".pth" || lower == ".bin" || lower == ".ckpt" {
			return "", fmt.Errorf("%w: %q is a pickle-backed torch file; export .safetensors instead", ErrUnsupportedState, extension)
		}
		return "", fmt.Errorf("%w: got extension %q", ErrUnsupportedState, extension)
	}
	name := strings.TrimSpace(strings.TrimSuffix(base, extension))
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: the upload has no name before %s", ErrUnsupportedState, StateExtension)
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%w: a voice name may not start with a dot", ErrUnsupportedState)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: a voice name may not contain a path separator", ErrUnsupportedState)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: a voice name may not contain control characters", ErrUnsupportedState)
		}
	}
	if runes := []rune(name); len(runes) > maxStateNameRunes {
		return "", fmt.Errorf("%w: a voice name is limited to %d characters", ErrUnsupportedState, maxStateNameRunes)
	}
	return name, nil
}

// BuiltinName reports whether a custom file would shadow a built-in voice.
func (s *Service) BuiltinName(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, builtin := range s.builtins {
		if strings.EqualFold(builtin, name) {
			return true
		}
	}
	return false
}

// existingState reports whether target is already a regular file, and rejects
// a case-only collision with another file: the scan is case-insensitive, so
// two names differing only by case would make one of them invisible.
func existingState(target, name string) (bool, error) {
	info, err := os.Lstat(target)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("voice: refusing to replace a non-regular file named %q", filepath.Base(target))
		}
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		// fall through to the case-insensitive duplicate check
	default:
		return false, fmt.Errorf("voice: inspect existing state: %w", err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		return false, fmt.Errorf("voice: read library: %w", err)
	}
	for _, entry := range entries {
		existing := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if strings.EqualFold(existing, name) {
			return false, fmt.Errorf("%w: %q differs only by capitalisation", ErrVoiceExists, existing)
		}
	}
	return false, nil
}

// stage copies one upload into a hidden temp file in the library folder. The
// copy is bounded before the limit is exceeded and synced so the bytes TTS
// validates are the bytes that will be renamed into place.
func (s *Service) stage(ctx context.Context, src io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(s.dir, ".upload-*"+StateExtension)
	if err != nil {
		return "", fmt.Errorf("voice: create upload staging file: %w", err)
	}
	tempPath := file.Name()
	cleanup := func() {
		file.Close()
		os.Remove(tempPath)
	}

	written, err := io.Copy(file, io.LimitReader(src, MaxStateBytes+1))
	if err != nil {
		cleanup()
		return "", fmt.Errorf("voice: read upload: %w", err)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return "", err
	}
	if written > MaxStateBytes {
		cleanup()
		return "", ErrStateTooLarge
	}
	if written == 0 {
		cleanup()
		return "", fmt.Errorf("%w: the upload is empty", ErrInvalidState)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("voice: sync upload: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(tempPath)
		return "", fmt.Errorf("voice: close upload: %w", err)
	}
	// The TTS container may run as a different user; the volume is shared, so
	// the published state must be readable without widening the directory.
	if err := os.Chmod(tempPath, 0o644); err != nil {
		os.Remove(tempPath)
		return "", fmt.Errorf("voice: set upload permissions: %w", err)
	}
	return tempPath, nil
}

// publishState renames the staged file into place. An existing target is moved
// to a hidden .bak first; the caller restores it on any later failure. Renames
// stay inside one directory, so the published name appears atomically.
func publishState(tempPath, target string) (backup string, err error) {
	info, statErr := os.Lstat(target)
	switch {
	case statErr == nil:
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("voice: refusing to replace a non-regular file named %q", filepath.Base(target))
		}
		backup, err = reserveBackup(filepath.Dir(target))
		if err != nil {
			return "", err
		}
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("voice: move existing voice state aside: %w", err)
		}
	case errors.Is(statErr, os.ErrNotExist):
		// No previous file, nothing to preserve.
	default:
		return "", fmt.Errorf("voice: inspect publish target: %w", statErr)
	}
	if err := os.Rename(tempPath, target); err != nil {
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return "", fmt.Errorf("voice: publish voice state: %w", err)
	}
	return backup, nil
}

// reserveBackup picks an unused hidden name with a non-state suffix, so the
// scanner ignores it while a replacement is in flight.
func reserveBackup(dir string) (string, error) {
	file, err := os.CreateTemp(dir, ".replace-*.bak")
	if err != nil {
		return "", fmt.Errorf("voice: reserve replacement backup: %w", err)
	}
	name := file.Name()
	file.Close()
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("voice: reserve replacement backup: %w", err)
	}
	return name, nil
}

// rollbackImport removes the new final file and restores the previous bytes
// and index row. It deliberately uses a fresh context: the request that
// triggered the failure may already be cancelled. Restoring the row directly
// (rather than rescanning) is what makes a failed replacement leave the old
// voice selectable even if the model is the thing that failed.
func (s *Service) rollbackImport(target, backup, name, remoteFinal string, previous Voice, hadPrevious bool, probe Probe) {
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Error("voice: remove failed upload", "file", filepath.Base(target), "error", err.Error())
	}
	if backup != "" {
		if err := os.Rename(backup, target); err != nil {
			s.logger.Error("voice: restore replaced voice", "file", filepath.Base(target), "error", err.Error())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A failed final validation can leave the rejected state in the TTS cache
	// under the final path. Evict it before restoring the index so the next
	// synthesis reloads the restored file rather than the failed bytes.
	if forget, ok := probe.(Forgetter); ok {
		if err := forget.Forget(ctx, remoteFinal); err != nil {
			s.logger.Warn("voice: clear failed state cache", "error", err.Error())
		}
	}
	if err := s.restoreIndexRow(ctx, name, previous, hadPrevious); err != nil {
		s.logger.Error("voice: restore index after rollback", "error", err.Error())
		if probe != nil {
			if err := s.Rescan(ctx, probe); err != nil {
				s.logger.Error("voice: rescan after rollback", "error", err.Error())
			}
		}
	}
}

// restoreIndexRow puts one library row back (or removes the just-added row)
// without touching any other file's entry.
func (s *Service) restoreIndexRow(ctx context.Context, name string, previous Voice, hadPrevious bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("voice: begin restore: %w", err)
	}
	defer tx.Rollback()
	if hadPrevious {
		validatedAt := ""
		if !previous.ValidatedAt.IsZero() {
			validatedAt = previous.ValidatedAt.UTC().Format(time.RFC3339)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO voices (name, file, size, mod_time, added_at, validated_revision, validated_at, stale, last_error, note)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET
			     file = excluded.file,
			     size = excluded.size,
			     mod_time = excluded.mod_time,
			     added_at = excluded.added_at,
			     validated_revision = excluded.validated_revision,
			     validated_at = excluded.validated_at,
			     stale = excluded.stale,
			     last_error = excluded.last_error,
			     note = excluded.note`,
			previous.Name, previous.File, previous.Size, previous.modTime, previous.AddedAt.UnixNano(),
			previous.ValidatedRevision, validatedAt, previous.Stale, previous.LastError, previous.Note); err != nil {
			return fmt.Errorf("voice: restore %s: %w", previous.Name, err)
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM voices WHERE name = ?`, name); err != nil {
		return fmt.Errorf("voice: drop failed upload %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("voice: commit restore: %w", err)
	}
	return s.reload(ctx)
}
