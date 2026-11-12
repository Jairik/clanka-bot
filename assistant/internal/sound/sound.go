// Package sound owns the Cue library and the fixed Cue roles. A Cue is a
// non-speech sound played for one role: wake detection, a Timer or Alarm Ring,
// a Reminder lead-in or the Unavailable message. The library combines the
// built-in chime that ships in the image with WAV files dropped into the data
// volume's sounds/ folder, including uploads canonicalised by the Dashboard;
// role assignments are ordinary live settings, so a Dashboard or voice write
// applies on the next play (ADR-0001).
package sound

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// Role is one fixed Cue role. The set is code, not configuration: a Ticket adds
// a role only when a consumer exists.
type Role string

const (
	// RoleWake plays when the wake word fires, before the user speaks.
	RoleWake Role = "wake"
	// RoleTimerRing is the sound a Timer Ring loops. #52 owns the Ring engine;
	// this ticket only supplies the resolved sound.
	RoleTimerRing Role = "timer_ring"
	// RoleAlarmRing is the sound an Alarm Ring loops (#52/#53).
	RoleAlarmRing Role = "alarm_ring"
	// RoleReminder plays before a Reminder's message (#54).
	RoleReminder Role = "reminder"
	// RoleError plays before the Unavailable message.
	RoleError Role = "error"
)

// Roles is the fixed role order used by the settings list.
var Roles = []Role{RoleWake, RoleTimerRing, RoleAlarmRing, RoleReminder, RoleError}

const (
	// BuiltinChime is the synthesised two-note chime that ships in the image.
	// It is the default for every role, including wake.
	BuiltinChime = "chime"
	// None is the explicit silent assignment.
	None = "none"

	// libraryDir is the sounds/ folder under the data volume.
	libraryDir = "sounds"
)

// Cap returns the maximum sound length a role accepts when it is assigned.
// The cap is checked on assignment and never trims a file.
func Cap(role Role) time.Duration {
	switch role {
	case RoleWake:
		// Endpointing waits for the wake Cue to finish, so a long sound delays
		// the start of the question.
		return time.Second
	case RoleReminder, RoleError:
		return 3 * time.Second
	case RoleTimerRing, RoleAlarmRing:
		return time.Minute
	default:
		return 0
	}
}

// Key is the live setting that stores one role's assignment.
func Key(role Role) string { return "sounds." + string(role) }

// Valid reports whether role is one of the fixed roles.
func Valid(role Role) bool {
	for _, known := range Roles {
		if role == known {
			return true
		}
	}
	return false
}

// Config is the config.yaml layer for the five roles.
type Config struct {
	Wake      string
	TimerRing string
	AlarmRing string
	Reminder  string
	Error     string
}

// value returns the config value for one role.
func (c Config) value(role Role) string {
	switch role {
	case RoleWake:
		return c.Wake
	case RoleTimerRing:
		return c.TimerRing
	case RoleAlarmRing:
		return c.AlarmRing
	case RoleReminder:
		return c.Reminder
	case RoleError:
		return c.Error
	default:
		return ""
	}
}

// Sound is one library entry.
type Sound struct {
	// Name is what a role assignment uses, without the .wav extension.
	Name string
	// Path is the absolute file path; empty for a built-in.
	Path string
	// Builtin marks a sound that ships in the image rather than the volume.
	Builtin  bool
	Duration time.Duration
	// AddedAt is when the name first entered the index. A rescan keeps it.
	AddedAt time.Time
}

// Clip is decoded, playback-ready mono PCM.
type Clip struct {
	PCM      []byte
	Format   audio.Format
	Duration time.Duration
}

// Sink is the one speaker stream. audio.Sink and speech.Sink both satisfy it.
type Sink interface {
	Open(ctx context.Context, format audio.Format) (io.WriteCloser, error)
}

// Play writes the clip and waits for it to drain. The caller's sink is
// normally the shared speech.SerialSink, so a Cue can never overlap a reply.
func (c Clip) Play(ctx context.Context, sink Sink) error {
	if len(c.PCM) == 0 {
		return nil
	}
	if sink == nil {
		return errors.New("sound: sink is required")
	}
	if err := audio.NewPCMPlayer(sink).Play(ctx, c.PCM, c.Format); err != nil {
		return fmt.Errorf("sound: play cue: %w", err)
	}
	return nil
}

// Service is the library index plus the live role resolution. It is safe for
// concurrent playback and settings writes.
type Service struct {
	volumeDir  string
	libraryDir string
	format     audio.Format
	logger     *slog.Logger
	db         *sql.DB

	mu      sync.RWMutex
	sounds  map[string]Sound
	builtin Clip
	chime   Sound

	settings *settings.Service
	sink     Sink
	cache    map[string]cachedClip

	// importMu serialises uploads so name selection and publish cannot race.
	importMu sync.Mutex
}

// cachedClip keeps one decoded file until its mtime or size changes.
type cachedClip struct {
	modTime time.Time
	size    int64
	clip    Clip
}

// New opens the library index under volumeDir, creates sounds/, indexes every
// valid WAV and the built-in chime, and returns the service. format is the
// mono PCM16 layout every Cue is rendered to; the wake path uses the capture
// format so the existing endpointing timing stays unchanged.
func New(ctx context.Context, st *store.Store, volumeDir string, format audio.Format, logger *slog.Logger) (*Service, error) {
	if st == nil || st.DB() == nil {
		return nil, errors.New("sound: store is required")
	}
	if format.Rate <= 0 || format.Width != 2 || format.Channels != 1 {
		return nil, fmt.Errorf("sound: cue format must be mono PCM16 with a positive rate, got %+v", format)
	}
	if logger == nil {
		logger = slog.Default()
	}
	absolute, err := filepath.Abs(volumeDir)
	if err != nil {
		return nil, fmt.Errorf("sound: resolve volume %s: %w", volumeDir, err)
	}
	dir := filepath.Join(absolute, libraryDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sound: create library %s: %w", dir, err)
	}
	chimePCM := audio.Chime(format.Rate)
	if len(chimePCM) == 0 {
		return nil, errors.New("sound: built-in chime is empty")
	}
	cues := &Service{
		volumeDir:  absolute,
		libraryDir: dir,
		format:     format,
		logger:     logger,
		db:         st.DB(),
		sounds:     make(map[string]Sound),
		builtin: Clip{
			PCM:      chimePCM,
			Format:   format,
			Duration: pcmDuration(chimePCM, format),
		},
		cache: make(map[string]cachedClip),
	}
	cues.chime = Sound{
		Name:     BuiltinChime,
		Builtin:  true,
		Duration: cues.builtin.Duration,
	}
	if err := cues.Rescan(ctx); err != nil {
		return nil, err
	}
	return cues, nil
}

// Bind sets the settings service and the sink. Both are optional: without a
// settings service a Clip resolves to none, and without a sink playback is a
// no-op for a text-only setup.
func (s *Service) Bind(svc *settings.Service, sink Sink) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = svc
	s.sink = sink
}

// Definitions builds the five live role settings. A non-empty config value
// becomes the config layer; empty leaves the built-in chime default. The
// validator runs for config and every write, and rejects a sound over the
// role's cap instead of trimming it.
func (s *Service) Definitions(cfg Config) []settings.Definition {
	defs := make([]settings.Definition, 0, len(Roles))
	for _, role := range Roles {
		role := role
		opts := []settings.Option{
			settings.WithDescription(fmt.Sprintf(
				"Sound played as the %s Cue: a library name, a path under the data volume, or none. Maximum %s.",
				role, Cap(role))),
			settings.WithValidator(func(value any) error {
				text, ok := value.(string)
				if !ok {
					return fmt.Errorf("sound: %s must be a sound name", role)
				}
				return s.Validate(role, text)
			}),
		}
		if value := strings.TrimSpace(cfg.value(role)); value != "" {
			opts = append(opts, settings.WithConfig(value))
		}
		defs = append(defs, settings.String(Key(role), BuiltinChime, opts...))
	}
	return defs
}

// Validate checks one assignment: it must be a known sound, a confined volume
// path or none, and its duration must fit the role's cap.
func (s *Service) Validate(role Role, value string) error {
	if !Valid(role) {
		return fmt.Errorf("sound: unknown Cue role %q", role)
	}
	entry, ok, err := s.Resolve(value)
	if err != nil {
		return err
	}
	if !ok {
		return nil // explicit none
	}
	if entry.Duration > Cap(role) {
		return fmt.Errorf(
			"sound: %q is %s, over the %s cap for the %s Cue; choose a shorter sound",
			entry.Name, roundDuration(entry.Duration), Cap(role), role)
	}
	return nil
}

// Resolve turns one assignment value into a library entry. It accepts a
// built-in name, an indexed library name, a .wav file name in the library, or
// a volume path; any path is confined to the data volume. The second result is
// false for none.
func (s *Service) Resolve(value string) (Sound, bool, error) {
	if s == nil {
		return Sound{}, false, errors.New("sound: library is not available")
	}
	trimmed := strings.TrimSpace(value)
	if strings.EqualFold(trimmed, None) {
		return Sound{}, false, nil
	}
	if trimmed == "" {
		return Sound{}, false, errors.New("sound: an assignment must be a sound name, a volume path or none")
	}
	if looksLikePath(trimmed) {
		path, err := s.authorizedPath(trimmed)
		if err != nil {
			return Sound{}, false, err
		}
		info, err := audio.InspectWAV(path)
		if err != nil {
			return Sound{}, false, fmt.Errorf("sound: %q: %w", trimmed, err)
		}
		return Sound{
			Name:     strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
			Path:     path,
			Duration: info.Duration,
			AddedAt:  time.Now().UTC(),
		}, true, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.EqualFold(trimmed, BuiltinChime) {
		return s.chime, true, nil
	}
	for name, entry := range s.sounds {
		if strings.EqualFold(name, trimmed) {
			return entry, true, nil
		}
	}
	return Sound{}, false, fmt.Errorf("sound: %q is not in the sound library", trimmed)
}

// List returns the indexed library sorted by name.
func (s *Service) List() []Sound {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Sound, 0, len(s.sounds))
	for _, entry := range s.sounds {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LibraryDir is where volume sounds live. #56 writes canonical WAV files here
// and calls Rescan.
func (s *Service) LibraryDir() string {
	if s == nil {
		return ""
	}
	return s.libraryDir
}

// Find returns an indexed library entry by name, without accepting paths. The
// Dashboard's browser and Pi previews use this narrower lookup so an HTTP name
// can never reach an arbitrary file in the data volume.
func (s *Service) Find(name string) (Sound, bool) {
	if s == nil {
		return Sound{}, false
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || looksLikePath(trimmed) {
		return Sound{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.EqualFold(trimmed, BuiltinChime) {
		return s.chime, true
	}
	for indexedName, entry := range s.sounds {
		if strings.EqualFold(indexedName, trimmed) {
			return entry, true
		}
	}
	return Sound{}, false
}

// File returns the on-disk WAV for a volume library sound, plus its mtime for
// conditional requests. The built-in chime has no file; callers fall back to
// Audio for it. Serving the file directly keeps a long browser preview out of
// memory.
func (s *Service) File(name string) (string, time.Time, bool) {
	entry, ok := s.Find(name)
	if !ok || entry.Builtin || entry.Path == "" {
		return "", time.Time{}, false
	}
	info, err := os.Stat(entry.Path)
	if err != nil || !info.Mode().IsRegular() {
		return "", time.Time{}, false
	}
	return entry.Path, info.ModTime(), true
}

// Audio returns one library sound as a canonical WAV for browser playback.
// The built-in chime has no file, so it is encoded from the in-memory PCM.
func (s *Service) Audio(name string) ([]byte, Sound, error) {
	entry, ok := s.Find(name)
	if !ok {
		return nil, Sound{}, fmt.Errorf("sound: %q is not in the sound library", name)
	}
	clip, err := s.load(entry)
	if err != nil {
		return nil, Sound{}, fmt.Errorf("sound: read %q: %w", entry.Name, err)
	}
	return audio.EncodeWAV(clip.PCM, clip.Format), entry, nil
}

// PlayName plays one library sound once through the bound sink. It is the Pi
// preview seam: the caller normally queues it, so it cannot cut a reply. No
// role cap applies because a preview is not an assignment.
func (s *Service) PlayName(ctx context.Context, name string) error {
	entry, ok := s.Find(name)
	if !ok {
		return fmt.Errorf("sound: %q is not in the sound library", name)
	}
	clip, err := s.load(entry)
	if err != nil {
		return fmt.Errorf("sound: read %q: %w", entry.Name, err)
	}
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil {
		return nil
	}
	return clip.Play(ctx, sink)
}

// Clip resolves a role through the live setting and returns playback-ready
// PCM. A none assignment, an unknown role, a missing file or an over-cap file
// returns false; it never trims.
func (s *Service) Clip(role Role) (Clip, bool) {
	if s == nil || !Valid(role) {
		return Clip{}, false
	}
	s.mu.RLock()
	settingsSvc := s.settings
	s.mu.RUnlock()
	if settingsSvc == nil {
		return Clip{}, false
	}
	return s.clipFor(role, settingsSvc.String(Key(role)))
}

// DefaultClip resolves a role's config-layer value before the live settings
// service exists, so a Timer recovered at startup still rings its sound.
func (s *Service) DefaultClip(cfg Config, role Role) (Clip, bool) {
	if s == nil || !Valid(role) {
		return Clip{}, false
	}
	value := strings.TrimSpace(cfg.value(role))
	if value == "" {
		value = BuiltinChime
	}
	return s.clipFor(role, value)
}

// clipFor resolves one assignment and loads it under the role cap.
func (s *Service) clipFor(role Role, value string) (Clip, bool) {
	entry, ok, err := s.Resolve(value)
	if err != nil {
		s.logger.Warn("sound cue unavailable", "role", string(role), "error", err.Error())
		return Clip{}, false
	}
	if !ok {
		return Clip{}, false
	}
	clip, err := s.load(entry)
	if err != nil {
		s.logger.Warn("sound cue unreadable", "role", string(role), "sound", entry.Name, "error", err.Error())
		return Clip{}, false
	}
	if clip.Duration > Cap(role) {
		// The file changed after assignment. Refuse to play rather than trim.
		s.logger.Warn("sound cue over cap",
			"role", string(role), "sound", entry.Name, "duration", clip.Duration.String(), "cap", Cap(role).String())
		return Clip{}, false
	}
	return clip, true
}

// Play resolves one role and plays it through the bound sink. A none
// assignment or a text-only setup is a quiet no-op.
func (s *Service) Play(ctx context.Context, role Role) error {
	if s == nil {
		return nil
	}
	clip, ok := s.Clip(role)
	if !ok {
		return nil
	}
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil {
		return nil
	}
	return clip.Play(ctx, sink)
}

// load returns the built-in chime or a decoded file, cached until the file
// changes. Decoded PCM is shared read-only by concurrent plays.
func (s *Service) load(entry Sound) (Clip, error) {
	if entry.Builtin || strings.EqualFold(entry.Name, BuiltinChime) {
		return s.builtin, nil
	}
	info, err := os.Stat(entry.Path)
	if err != nil {
		return Clip{}, err
	}
	s.mu.RLock()
	cached, ok := s.cache[entry.Path]
	s.mu.RUnlock()
	if ok && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached.clip, nil
	}
	decoded, err := audio.ReadWAV(entry.Path)
	if err != nil {
		return Clip{}, err
	}
	pcm := decoded.PCM
	if decoded.Format.Rate != s.format.Rate {
		pcm = audio.ResamplePCM(pcm, decoded.Format.Rate, s.format.Rate)
		if len(pcm) == 0 {
			return Clip{}, errors.New("resampled audio is empty")
		}
	}
	clip := Clip{
		PCM:      pcm,
		Format:   s.format,
		Duration: pcmDuration(pcm, s.format),
	}
	s.mu.Lock()
	s.cache[entry.Path] = cachedClip{modTime: info.ModTime(), size: info.Size(), clip: clip}
	s.mu.Unlock()
	return clip, nil
}

// pcmDuration is the wall-clock length of mono PCM16.
func pcmDuration(pcm []byte, format audio.Format) time.Duration {
	byteRate := format.Rate * format.Width * format.Channels
	if byteRate <= 0 {
		return 0
	}
	return time.Duration(len(pcm)) * time.Second / time.Duration(byteRate)
}

// roundDuration renders a cap error in a stable, readable form.
func roundDuration(d time.Duration) string {
	return d.Round(10 * time.Millisecond).String()
}

// looksLikePath reports whether a value names a file rather than a library
// name. A bare name never needs a separator and never ends in .wav.
func looksLikePath(value string) bool {
	return strings.ContainsAny(value, `/\`) || strings.HasSuffix(strings.ToLower(value), ".wav")
}

// authorizedPath resolves a config or stored path and confines it to the data
// volume. Relative values are tried in sounds/ first, then at the volume root,
// so both "birds.wav" and "sounds/birds.wav" work from config.yaml. Symlinks
// are resolved before the containment check so they cannot escape.
func (s *Service) authorizedPath(value string) (string, error) {
	candidates := []string{value}
	if !filepath.IsAbs(value) {
		candidates = []string{
			filepath.Join(s.libraryDir, value),
			filepath.Join(s.volumeDir, value),
		}
	}
	var lastErr error
	for _, candidate := range candidates {
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		real, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			lastErr = err
			continue
		}
		if !within(s.volumeDir, real) {
			return "", fmt.Errorf("sound: %q is outside the data volume", value)
		}
		info, err := os.Stat(real)
		if err != nil {
			lastErr = err
			continue
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("sound: %q is not a regular file", value)
		}
		return real, nil
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return "", fmt.Errorf("sound: %q is not a readable file in the data volume: %w", value, lastErr)
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
