// Package tz resolves the one timezone the assistant uses to turn a spoken
// clock time into an instant. The `timezone` setting is live: a Dashboard or
// voice write through the shared settings service applies on the next Alarm or
// Reminder without a restart. When the setting is unset, TZ is used; when that
// is unset too, UTC is the safe fallback and the caller logs the startup
// warning. Nothing here guesses a timezone from an IP address.
package tz

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	// Embed the IANA database so the arm64 image needs no system tzdata.
	_ "time/tzdata"

	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Key is the settings key and the config.yaml field name.
const Key = "timezone"

// Reader is the settings read seam. *settings.Service satisfies it.
type Reader interface {
	String(key string) string
}

// Definition registers the timezone setting with the same service the
// Dashboard and the voice Tools write through. An empty value means "unset":
// TZ, then UTC.
func Definition(configValue string) settings.Definition {
	opts := []settings.Option{
		settings.WithKind(settings.Live),
		settings.WithValidator(validateSetting),
		settings.WithDescription("IANA timezone for clock-time Alarms and Reminders; empty uses TZ, then UTC."),
	}
	// An empty config value means "unset", so it must not shadow the default
	// layer with an empty source=config value.
	if strings.TrimSpace(configValue) != "" {
		opts = append(opts, settings.WithConfig(configValue))
	}
	return settings.String(Key, "", opts...)
}

// validateSetting rejects anything time.LoadLocation would not resolve as an
// IANA zone. An empty value is allowed and means the fallback chain.
func validateSetting(value any) error {
	name, ok := value.(string)
	if !ok {
		return fmt.Errorf("want a timezone name")
	}
	return Validate(name)
}

// Validate reports whether name is empty or a known IANA timezone. The settings
// definition and config.yaml use the same check.
func Validate(name string) error {
	_, err := Load(name)
	return err
}

// Load resolves one IANA timezone name. "" returns (nil, nil) so callers can
// treat "unset" separately from an invalid name.
func Load(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	if name == "Local" {
		return nil, fmt.Errorf("%q is not an IANA timezone name", name)
	}
	// LoadLocation treats a path as a zoneinfo file. Alert settings must name a
	// zone, so a path (or parent traversal) is rejected here.
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return nil, fmt.Errorf("%q is not an IANA timezone name", name)
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a known IANA timezone: %w", name, err)
	}
	return location, nil
}

// Service resolves the effective timezone. Bind installs the settings reader
// after the settings service exists, because Tool modules register before it;
// reads before Bind use TZ and then UTC.
type Service struct {
	env    string
	logger *slog.Logger

	mu     sync.Mutex
	reader Reader
	cached string
	loaded *time.Location
}

// New captures the process environment fallback. main passes os.Getenv("TZ").
func New(env string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{env: strings.TrimSpace(env), logger: logger}
}

// Bind attaches the shared settings service. The current value is read on
// every Location call, so this is a live seam, not a one-time snapshot.
func (s *Service) Bind(reader Reader) {
	s.mu.Lock()
	s.reader = reader
	s.cached = ""
	s.loaded = nil
	s.mu.Unlock()
}

// Location returns the effective zone. A stored or config value wins, then
// TZ, then UTC. An invalid value that escaped validation is logged and skipped
// rather than stopping an alert from firing.
func (s *Service) Location() *time.Location {
	name := s.Name()
	location, err := Load(name)
	if err != nil || location == nil {
		if err != nil {
			s.logger.Warn("timezone fallback", "name", name, "error", err.Error())
		}
		return time.UTC
	}
	return location
}

// Name returns the effective timezone name, or "UTC" when nothing is set.
func (s *Service) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader != nil {
		if name := strings.TrimSpace(s.reader.String(Key)); name != "" {
			return s.validLocked(name)
		}
	}
	if s.env != "" {
		return s.validLocked(s.env)
	}
	return "UTC"
}

// IsUTC reports whether the effective zone is UTC, so startup can warn once.
func (s *Service) IsUTC() bool { return s.Name() == "UTC" }

// ResolveWall turns a local wall-clock time into an instant. A normal time and
// a fall-back overlap resolve to the first occurrence. A spring-forward gap
// has no such instant; the post-transition reading is used, so 02:30 becomes
// 03:30 when the clock jumps over it. Alarm and Reminder share this rule, so a
// clock-time alert behaves the same in either Tool.
func ResolveWall(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	// The nominal wall time as if it were UTC; subtracting a zone offset gives
	// the real instant without time.Date's unspecified overlap choice.
	nominal := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	offsets := make(map[int]bool)
	for _, probe := range []time.Duration{
		-48 * time.Hour, -24 * time.Hour, -12 * time.Hour,
		12 * time.Hour, 24 * time.Hour, 48 * time.Hour,
	} {
		_, offset := nominal.Add(probe).In(loc).Zone()
		offsets[offset] = true
	}
	var candidates []time.Time
	for offset := range offsets {
		instant := nominal.Add(-time.Duration(offset) * time.Second)
		local := instant.In(loc)
		if local.Year() == year && local.Month() == month && local.Day() == day &&
			local.Hour() == hour && local.Minute() == minute {
			candidates = append(candidates, instant)
		}
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
		return candidates[0]
	}
	// Gap: no instant carries the requested wall time. Use the later reading,
	// which is the post-transition offset, so the alert fires after the jump.
	var latest time.Time
	for offset := range offsets {
		instant := nominal.Add(-time.Duration(offset) * time.Second)
		if instant.After(latest) {
			latest = instant
		}
	}
	return latest
}

// validLocked returns name when it loads, keeping a small cache so repeated
// scheduling does not re-read zoneinfo. The caller holds the write lock.
func (s *Service) validLocked(name string) string {
	if name == s.cached && s.loaded != nil {
		return name
	}
	location, err := Load(name)
	if err != nil {
		s.logger.Warn("timezone ignored", "name", name, "error", err.Error())
		s.cached, s.loaded = "", nil
		return "UTC"
	}
	s.cached, s.loaded = name, location
	return name
}
