package sound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
)

// ErrUploadTooLarge reports an upload over the 10 MB request limit. The HTTP
// layer maps it to 413; every other import error is a 400.
var ErrUploadTooLarge = errors.New("sound: upload is larger than the 10 MB limit")

// reservedNames would be shadowed by the built-in chime or the none sentinel,
// so an upload never takes them.
var reservedNames = []string{BuiltinChime, None}

// Import validates and canonicalises one uploaded WAV or MP3 into the library.
// The upload is bounded at 10 MB and streamed to a temporary file; the
// canonical WAV is written beside the library as a hidden temp and renamed into
// place only after decoding succeeds. A failed import leaves neither a library
// file nor an index row. Role length caps are deliberately not checked here:
// they apply when a sound is assigned.
func (s *Service) Import(ctx context.Context, src io.Reader, filename string) (Sound, error) {
	if s == nil || s.db == nil {
		return Sound{}, errors.New("sound: library is not open")
	}
	if src == nil {
		return Sound{}, errors.New("sound: upload is empty")
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()

	upload, err := os.CreateTemp(s.libraryDir, ".upload-*")
	if err != nil {
		return Sound{}, fmt.Errorf("sound: create upload spool: %w", err)
	}
	uploadPath := upload.Name()
	defer func() {
		upload.Close()
		os.Remove(uploadPath)
	}()

	written, err := io.Copy(upload, io.LimitReader(src, audio.MaxUploadBytes+1))
	if err != nil {
		return Sound{}, fmt.Errorf("sound: read upload: %w", err)
	}
	if written > audio.MaxUploadBytes {
		return Sound{}, ErrUploadTooLarge
	}
	if written == 0 {
		return Sound{}, errors.New("sound: upload is empty")
	}
	if err := upload.Sync(); err != nil {
		return Sound{}, fmt.Errorf("sound: sync upload: %w", err)
	}
	if err := upload.Close(); err != nil {
		return Sound{}, fmt.Errorf("sound: close upload: %w", err)
	}

	staged, err := os.CreateTemp(s.libraryDir, ".publish-*")
	if err != nil {
		return Sound{}, fmt.Errorf("sound: create publish spool: %w", err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	if err := staged.Close(); err != nil {
		return Sound{}, fmt.Errorf("sound: close publish spool: %w", err)
	}
	// Canonicalize creates its output exclusively, so reserve only the name.
	if err := os.Remove(stagedPath); err != nil {
		return Sound{}, fmt.Errorf("sound: reset publish spool: %w", err)
	}

	if _, err := audio.Canonicalize(ctx, uploadPath, stagedPath, s.format); err != nil {
		s.logger.Warn("sound upload rejected", "file", filepath.Base(filename), "error", err.Error())
		return Sound{}, uploadFailure(filename, err)
	}

	// Pick the name after decoding so an external drop during a long decode
	// keeps its file and this upload takes the next free name.
	name := s.availableName(filename)
	output := filepath.Join(s.libraryDir, name+".wav")
	if !within(s.libraryDir, output) {
		return Sound{}, fmt.Errorf("sound: refusing to publish outside %s", s.libraryDir)
	}
	if err := os.Rename(stagedPath, output); err != nil {
		return Sound{}, fmt.Errorf("sound: publish %s: %w", name, err)
	}

	if err := s.Rescan(ctx); err != nil {
		// Rescan is transactional, so dropping the just-published file restores
		// the library to the index instead of leaving a partial import.
		os.Remove(output)
		_ = s.Rescan(ctx)
		return Sound{}, fmt.Errorf("sound: index upload: %w", err)
	}
	entry, ok := s.Find(name)
	if !ok {
		os.Remove(output)
		return Sound{}, fmt.Errorf("sound: upload %q did not enter the library", name)
	}
	return entry, nil
}

// uploadFailure turns a decoder error into a short, user-facing reason. The
// full error stays in the log; the HTTP page never echoes a temp path.
func uploadFailure(filename string, err error) error {
	name := []rune(filepath.Base(filename))
	if len(name) > 80 {
		name = name[:80]
	}
	reason := "is not a valid WAV or MP3 file"
	message := err.Error()
	switch {
	case strings.Contains(message, "no audio frames"):
		reason = "contains no audio frames"
	case strings.Contains(message, "truncated"):
		reason = "is truncated"
	case strings.Contains(message, "unsupported"):
		reason = "uses an unsupported WAV format"
	case strings.Contains(message, "no fmt chunk"), strings.Contains(message, "no data chunk"):
		reason = "is not a valid WAV file"
	}
	return fmt.Errorf("sound: %q %s", string(name), reason)
}

// availableName turns an upload's original name into a safe library name. The
// base is slugged to letters, digits and single dashes, reserved names and
// collisions get a numeric suffix, and the result never contains a path
// separator.
func (s *Service) availableName(filename string) string {
	base := slugName(filename)
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		if s.nameTaken(candidate) {
			continue
		}
		return candidate
	}
}

// slugName keeps a display-friendly name while removing anything that could
// become a path or an awkward URL.
func slugName(filename string) string {
	base := filepath.Base(strings.TrimSpace(filename))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	pendingDash := false
	for _, r := range base {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
		default:
			pendingDash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if runes := []rune(name); len(runes) > 64 {
		name = strings.TrimRight(string(runes[:64]), "-")
	}
	if name == "" {
		return "sound"
	}
	return name
}

// nameTaken reports a collision with a reserved name, the in-memory index or a
// file already in the flat library folder. It is case-insensitive on every
// platform so the same name cannot be indexed twice.
func (s *Service) nameTaken(name string) bool {
	for _, reserved := range reservedNames {
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	for _, entry := range s.List() {
		if strings.EqualFold(entry.Name, name) {
			return true
		}
	}
	entries, err := os.ReadDir(s.libraryDir)
	if err != nil {
		return true // fail closed: do not overwrite when the folder is unreadable
	}
	for _, entry := range entries {
		existing := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if strings.EqualFold(existing, name) {
			return true
		}
	}
	return false
}
