// Package quiet owns Quiet hours: one daily window, an optional timed
// do-not-disturb period, and the current bypass set by "I'm awake". It reads
// the same live settings service the Dashboard and the voice Tools write, so a
// change applies to the next request without a restart. The volume is applied
// in software to PCM, never by changing the media/sink volume, so there is no
// host volume to restore. Alarms are exempt.
package quiet

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
)

// Settings keys. They are ordinary live settings: the Dashboard edits them and
// the voice Tools write the same overrides.
const (
	// EnabledKey turns the daily window on or off.
	EnabledKey = "quiet.enabled"
	// StartKey is the window start as a local "HH:MM" clock time.
	StartKey = "quiet.start"
	// EndKey is the window end as a local "HH:MM" clock time. A value earlier
	// than StartKey makes the window cross midnight.
	EndKey = "quiet.end"
	// VolumeKey is the absolute speech/Cue volume inside Quiet hours.
	VolumeKey = "quiet.volume"
	// DNDUntilKey is an absolute RFC3339 instant for a timed do-not-disturb
	// period; empty means none. It is independent of the daily window.
	DNDUntilKey = "quiet.dnd_until"
	// AwakeUntilKey is an absolute RFC3339 instant that bypasses the rest of
	// the current daily window; empty means no bypass.
	AwakeUntilKey = "quiet.awake_until"
)

const (
	// DefaultStart is the shipped window start (10:30 pm).
	DefaultStart = "22:30"
	// DefaultEnd is the shipped window end (7:00 am).
	DefaultEnd = "07:00"
	// DefaultVolume is the shipped quiet volume. It is absolute, not relative
	// to the current media volume.
	DefaultVolume = 35
	// NormalVolume is the gain used when Quiet hours are not active.
	NormalVolume = 100
)

// Config is the config.yaml layer of the Quiet settings.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Start   string `yaml:"start"`
	End     string `yaml:"end"`
	Volume  int    `yaml:"volume"`
}

// DefaultConfig is the shipped Quiet configuration.
func DefaultConfig() Config {
	return Config{Enabled: false, Start: DefaultStart, End: DefaultEnd, Volume: DefaultVolume}
}

// ParseClock reads one local clock time. It accepts "22:30", "7", "7:05",
// "10 pm", "10:30pm" and normalises the result to 24-hour "HH:MM". A bare hour
// must be 0-23 (or 1-12 with am/pm).
func ParseClock(text string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(text, "."), ",")))
	if value == "" {
		return "", false
	}
	meridiem := ""
	switch {
	case strings.HasSuffix(value, "am"), strings.HasSuffix(value, "a.m"):
		meridiem = "am"
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(value, "am"), "a.m"))
	case strings.HasSuffix(value, "pm"), strings.HasSuffix(value, "p.m"):
		meridiem = "pm"
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(value, "pm"), "p.m"))
	}
	hourText, minuteText := value, ""
	if before, after, found := strings.Cut(value, ":"); found {
		hourText, minuteText = strings.TrimSpace(before), strings.TrimSpace(after)
	}
	hour, err := strconv.Atoi(hourText)
	if err != nil {
		return "", false
	}
	minute := 0
	if minuteText != "" {
		parsed, err := strconv.Atoi(minuteText)
		if err != nil || parsed < 0 || parsed > 59 || len(minuteText) != 2 {
			return "", false
		}
		minute = parsed
	}
	if meridiem != "" {
		if hour < 1 || hour > 12 {
			return "", false
		}
		if meridiem == "pm" && hour != 12 {
			hour += 12
		}
		if meridiem == "am" && hour == 12 {
			hour = 0
		}
	} else if hour < 0 || hour > 23 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), true
}

// ValidateWindow checks a pair of clock strings. Start and end must differ:
// an all-day window would make every Announcement wait forever.
func ValidateWindow(start, end string) error {
	normalizedStart, ok := ParseClock(start)
	if !ok {
		return fmt.Errorf("quiet: start must be a clock time such as 22:30")
	}
	normalizedEnd, ok := ParseClock(end)
	if !ok {
		return fmt.Errorf("quiet: end must be a clock time such as 07:00")
	}
	if normalizedStart == normalizedEnd {
		return fmt.Errorf("quiet: start and end must differ")
	}
	return nil
}

// Definitions builds the live Quiet settings for the one settings service.
// The dnd/awake instants are written only by the voice Tools, but they are
// ordinary settings so they survive a restart and can be inspected.
func Definitions(cfg Config) []settings.Definition {
	if cfg.Start == "" {
		cfg.Start = DefaultStart
	}
	if cfg.End == "" {
		cfg.End = DefaultEnd
	}
	// Trust code defaults, but keep the config layer honest at startup.
	start, end := cfg.Start, cfg.End
	if value, ok := ParseClock(start); ok {
		start = value
	}
	if value, ok := ParseClock(end); ok {
		end = value
	}
	return []settings.Definition{
		settings.Bool(EnabledKey, false,
			settings.WithConfig(cfg.Enabled),
			settings.WithDescription("Hold Announcements, mute Notifications and speak at quiet.volume during the daily window."),
		),
		settings.String(StartKey, DefaultStart,
			settings.WithConfig(start),
			settings.WithValidator(validClock("start")),
			settings.WithDescription("Daily Quiet hours start in the effective timezone, as HH:MM. An end earlier than the start crosses midnight."),
		),
		settings.String(EndKey, DefaultEnd,
			settings.WithConfig(end),
			settings.WithValidator(validClock("end")),
			settings.WithDescription("Daily Quiet hours end in the effective timezone, as HH:MM."),
		),
		settings.Int(VolumeKey, DefaultVolume,
			settings.WithConfig(cfg.Volume),
			settings.WithValidator(validVolume),
			settings.WithDescription("Absolute reply, Filler, Timer and Reminder volume percent inside Quiet hours. Alarms are exempt."),
		),
		settings.String(DNDUntilKey, "",
			settings.WithValidator(validInstant("dnd_until")),
			settings.WithDescription("End of a timed do-not-disturb period as RFC3339 UTC; empty means none. Set by the quiet.dnd Tool."),
		),
		settings.String(AwakeUntilKey, "",
			settings.WithValidator(validInstant("awake_until")),
			settings.WithDescription("End of the current daily window bypassed by quiet.awake, as RFC3339 UTC; empty means none."),
		),
	}
}

// validClock rejects a stored or Dashboard value that is not a clock time.
func validClock(field string) func(any) error {
	return func(value any) error {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("quiet: %s must be a clock time", field)
		}
		if _, ok := ParseClock(text); !ok {
			return fmt.Errorf("quiet: %s must be a clock time such as 22:30", field)
		}
		return nil
	}
}

// validVolume allows an explicit mute through the full audible range.
func validVolume(value any) error {
	volume, ok := value.(int)
	if !ok {
		return fmt.Errorf("quiet: volume must be an integer percent")
	}
	if volume < 0 || volume > 100 {
		return fmt.Errorf("quiet: volume must be between 0 and 100")
	}
	return nil
}

// validInstant accepts an empty value or an RFC3339 timestamp.
func validInstant(field string) func(any) error {
	return func(value any) error {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("quiet: %s must be an RFC3339 time", field)
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Errorf("quiet: %s must be an RFC3339 time: %w", field, err)
		}
		return nil
	}
}

// Reader is the settings read seam. *settings.Service satisfies it. A nil or
// partially bound reader falls back to the code defaults, so a startup Ring
// before the late settings bind never blasts a surprise volume.
type Reader interface {
	Get(key string) (settings.Value, bool)
}

// Writer is the settings write seam. *settings.Service satisfies it. The voice
// Tools write the same stored overrides as the Dashboard.
type Writer interface {
	SetMany(ctx context.Context, values map[string]any) error
}

// Options configures a Service. Now and Location are injectable so window,
// DST and midnight behaviour are deterministic in tests.
type Options struct {
	Reader   Reader
	Now      func() time.Time
	Location func() *time.Location
	Logger   *slog.Logger
}

// Service evaluates Quiet hours against the live settings. It is safe for
// concurrent use; a settings write takes effect on the next Status read.
type Service struct {
	mu       sync.RWMutex
	reader   Reader
	writer   Writer
	now      func() time.Time
	location func() *time.Location
	logger   *slog.Logger
}

// New builds a Quiet service. A nil clock uses time.Now; a nil location uses
// the clock's own location.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{
		reader:   opts.Reader,
		now:      opts.Now,
		location: opts.Location,
		logger:   opts.Logger,
	}
}

// Bind attaches or replaces the settings reader. main binds a small startup
// reader before Tool registration and rebinds the full live service after it,
// so recovery already sees saved Quiet/Ring overrides.
func (s *Service) Bind(reader Reader) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.reader = reader
	s.mu.Unlock()
}

// BindWriter attaches the settings writer the voice Tools use. It is the same
// service the Dashboard writes, so there is one stored override per key.
func (s *Service) BindWriter(writer Writer) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.writer = writer
	s.mu.Unlock()
}

// writerValue returns the bound writer, or nil before the late bind.
func (s *Service) writerValue() Writer {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writer
}

// ErrNoWriter reports that a voice write arrived before the settings service
// was attached. In production the HTTP servers start after the bind, so this
// only protects custom wiring.
var ErrNoWriter = fmt.Errorf("quiet: settings writer is not available")

// SetWindow stores the daily window and turns Quiet hours on. It writes the
// same overrides the Dashboard edits. A currently active bypass is cleared so
// the user's new window takes effect.
func (s *Service) SetWindow(ctx context.Context, start, end string) (Status, error) {
	startValue, startOK := ParseClock(start)
	endValue, endOK := ParseClock(end)
	if !startOK || !endOK {
		return Status{}, fmt.Errorf("quiet: start and end must be clock times such as 22:30")
	}
	if err := ValidateWindow(startValue, endValue); err != nil {
		return Status{}, err
	}
	writer := s.writerValue()
	if writer == nil {
		return Status{}, ErrNoWriter
	}
	if err := writer.SetMany(ctx, map[string]any{
		EnabledKey:    true,
		StartKey:      startValue,
		EndKey:        endValue,
		AwakeUntilKey: "",
	}); err != nil {
		return Status{}, err
	}
	return s.Status(), nil
}

// Disable turns the daily window off and clears the timed do-not-disturb and
// awake overrides. "I'm awake" is a narrower bypass; this is the full off.
func (s *Service) Disable(ctx context.Context) error {
	writer := s.writerValue()
	if writer == nil {
		return ErrNoWriter
	}
	return writer.SetMany(ctx, map[string]any{
		EnabledKey:    false,
		DNDUntilKey:   "",
		AwakeUntilKey: "",
	})
}

// StartDND sets an absolute timed do-not-disturb end. It is independent of the
// daily window switch and clears an awake bypass so the new request wins.
func (s *Service) StartDND(ctx context.Context, duration time.Duration) (time.Time, error) {
	if duration <= 0 {
		return time.Time{}, fmt.Errorf("quiet: do not disturb needs a positive duration")
	}
	writer := s.writerValue()
	if writer == nil {
		return time.Time{}, ErrNoWriter
	}
	until := s.nowValue().Add(duration).UTC().Truncate(time.Second)
	if err := writer.SetMany(ctx, map[string]any{
		DNDUntilKey:   until.Format(time.RFC3339),
		AwakeUntilKey: "",
	}); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// Awake ends the current quiet period without turning the daily schedule off.
// Inside a window it bypasses the rest of that window (a reasonable
// current-window bypass); a timed do-not-disturb period is cancelled. Outside
// a window it only clears do-not-disturb. The status before the write is
// returned for the spoken confirmation.
func (s *Service) Awake(ctx context.Context) (Status, error) {
	writer := s.writerValue()
	if writer == nil {
		return Status{}, ErrNoWriter
	}
	before := s.Status()
	awakeUntil := ""
	if before.InWindow {
		awakeUntil = before.WindowEnd.UTC().Format(time.RFC3339)
	}
	if err := writer.SetMany(ctx, map[string]any{
		DNDUntilKey:   "",
		AwakeUntilKey: awakeUntil,
	}); err != nil {
		return Status{}, err
	}
	return before, nil
}

// Reason explains the current status.
type Reason string

const (
	// ReasonOff means Quiet hours are not active.
	ReasonOff Reason = "off"
	// ReasonWindow means the daily window is holding.
	ReasonWindow Reason = "window"
	// ReasonDND means a timed do-not-disturb period is holding.
	ReasonDND Reason = "do not disturb"
	// ReasonAwake means the user ended the current window early.
	ReasonAwake Reason = "awake"
)

// Status is one evaluation of the live settings at an instant.
type Status struct {
	// Quiet is true while Announcements are held and Notifications are muted.
	Quiet bool
	// Reason names why Quiet is on or why the window is not holding.
	Reason Reason
	// Enabled is the daily-window switch.
	Enabled bool
	// Start and End are the effective normalised clock strings.
	Start string
	End   string
	// Volume is the absolute quiet volume percent.
	Volume int
	// InWindow reports that the wall-clock instant falls inside the daily
	// window, even when Awake is bypassing it.
	InWindow bool
	// Bypassed reports that AwakeUntil is suppressing an otherwise active
	// window.
	Bypassed bool
	// WindowStart and WindowEnd are the containing window's instants. They are
	// zero when InWindow is false.
	WindowStart time.Time
	WindowEnd   time.Time
	// NextStart is the next daily window start after the evaluated instant.
	NextStart time.Time
	// DNDUntil and AwakeUntil are the stored instants, zero when absent.
	DNDUntil   time.Time
	AwakeUntil time.Time
	// NextChange is the earliest future instant that can change Quiet. It is
	// zero when the settings say nothing will change without a write.
	NextChange time.Time
}

// read resolves the live layers. Invalid stored values are impossible through
// a validated write; a corrupt value falls back to the code default and is
// logged rather than stopping the Assistant.
func (s *Service) read() (enabled bool, start, end string, volume int, dndUntil, awakeUntil time.Time) {
	start, end, volume = DefaultStart, DefaultEnd, DefaultVolume
	dndUntil, awakeUntil = time.Time{}, time.Time{}
	if s == nil {
		return
	}
	s.mu.RLock()
	reader := s.reader
	s.mu.RUnlock()
	if reader == nil {
		return
	}
	if value, ok := reader.Get(EnabledKey); ok {
		if flag, ok := value.Value.(bool); ok {
			enabled = flag
		}
	}
	if value, ok := reader.Get(StartKey); ok {
		if text, ok := value.Value.(string); ok {
			if normalized, ok := ParseClock(text); ok {
				start = normalized
			}
		}
	}
	if value, ok := reader.Get(EndKey); ok {
		if text, ok := value.Value.(string); ok {
			if normalized, ok := ParseClock(text); ok {
				end = normalized
			}
		}
	}
	if value, ok := reader.Get(VolumeKey); ok {
		if number, ok := value.Value.(int); ok && number >= 0 && number <= 100 {
			volume = number
		}
	}
	dndUntil = readInstant(reader, DNDUntilKey)
	awakeUntil = readInstant(reader, AwakeUntilKey)
	return
}

// readInstant resolves one stored RFC3339 instant, ignoring an unparseable
// value so a bad row cannot hold Quiet hours forever.
func readInstant(reader Reader, key string) time.Time {
	value, ok := reader.Get(key)
	if !ok {
		return time.Time{}
	}
	text, ok := value.Value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return time.Time{}
	}
	instant, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return instant
}

// nowValue returns the injected clock.
func (s *Service) nowValue() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

// locationValue returns the effective timezone for a clock instant.
func (s *Service) locationValue(at time.Time) *time.Location {
	if s != nil && s.location != nil {
		if location := s.location(); location != nil {
			return location
		}
	}
	return at.Location()
}

// clockPair parses the effective window into minutes since local midnight.
func clockPair(start, end string) (startMinute, endMinute int, ok bool) {
	startMinute, startOK := clockMinutes(start)
	endMinute, endOK := clockMinutes(end)
	if !startOK || !endOK || startMinute == endMinute {
		return 0, 0, false
	}
	return startMinute, endMinute, true
}

// clockMinutes parses one normalised HH:MM string.
func clockMinutes(text string) (int, bool) {
	hourText, minuteText, found := strings.Cut(text, ":")
	if !found {
		return 0, false
	}
	hour, err := strconv.Atoi(hourText)
	if err != nil || hour < 0 || hour > 23 {
		return 0, false
	}
	minute, err := strconv.Atoi(minuteText)
	if err != nil || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// windowBounds returns the containing window instants, or ok=false when at is
// outside it. It works in local wall-clock time, so an overnight window and
// both DST transitions behave as the user sees them.
func windowBounds(at time.Time, location *time.Location, startMinute, endMinute int) (time.Time, time.Time, bool) {
	local := at.In(location)
	year, month, day := local.Date()
	atStart := func(dayOffset int, minute int) time.Time {
		// Construct the wall-clock minute directly; adding a Duration across a
		// DST change would shift 07:00 to 08:00 on the spring-forward day.
		return time.Date(year, month, day+dayOffset, minute/60, minute%60, 0, 0, location)
	}
	// The wall-clock minute decides which side of a boundary the instant falls
	// on; time.Date then builds the actual instants with the right DST offset.
	localMinute := local.Hour()*60 + local.Minute()
	if startMinute < endMinute {
		if localMinute >= startMinute && localMinute < endMinute {
			return atStart(0, startMinute), atStart(0, endMinute), true
		}
		return time.Time{}, time.Time{}, false
	}
	// An overnight window can have started yesterday (before local midnight)
	// or today (after the start clock). at.Before is not used across dates so
	// the decision stays in wall-clock minutes, then the instants are built
	// with time.Date so DST offsets are correct.
	if localMinute < endMinute {
		return atStart(-1, startMinute), atStart(0, endMinute), true
	}
	if localMinute >= startMinute {
		return atStart(0, startMinute), atStart(1, endMinute), true
	}
	return time.Time{}, time.Time{}, false
}

// nextWindowStart returns the next daily window start strictly after at.
func nextWindowStart(at time.Time, location *time.Location, startMinute int) time.Time {
	local := at.In(location)
	year, month, day := local.Date()
	for offset := 0; offset <= 2; offset++ {
		candidate := time.Date(year, month, day+offset, startMinute/60, startMinute%60, 0, 0, location)
		if candidate.After(at) {
			return candidate
		}
	}
	return time.Time{}
}

// StatusAt evaluates the live settings at one instant. The caller's clock
// location is ignored in favour of the injected timezone seam.
func (s *Service) StatusAt(at time.Time) Status {
	if at.IsZero() {
		at = s.nowValue()
	}
	enabled, start, end, volume, dndUntil, awakeUntil := s.read()
	location := s.locationValue(at)
	status := Status{
		Enabled:    enabled,
		Start:      start,
		End:        end,
		Volume:     volume,
		DNDUntil:   dndUntil,
		AwakeUntil: awakeUntil,
		Reason:     ReasonOff,
	}
	if dndUntil.After(at) {
		status.Quiet = true
		status.Reason = ReasonDND
	}

	startMinute, endMinute, windowOK := clockPair(start, end)
	if windowOK {
		if windowStart, windowEnd, inWindow := windowBounds(at, location, startMinute, endMinute); inWindow {
			status.InWindow = true
			status.WindowStart = windowStart
			status.WindowEnd = windowEnd
		}
		status.NextStart = nextWindowStart(at, location, startMinute)
		if enabled && status.InWindow {
			if awakeUntil.After(at) {
				status.Bypassed = true
				if !status.Quiet {
					status.Reason = ReasonAwake
				}
			} else if !status.Quiet {
				status.Quiet = true
				status.Reason = ReasonWindow
			}
		}
	}

	// The next change is the earliest future boundary from every source, so
	// the controller can sleep until then instead of polling tightly.
	var next time.Time
	consider := func(candidate time.Time) {
		if candidate.After(at) && (next.IsZero() || candidate.Before(next)) {
			next = candidate
		}
	}
	if enabled {
		consider(status.NextStart)
		if status.InWindow {
			consider(status.WindowEnd)
		}
	}
	consider(dndUntil)
	consider(awakeUntil)
	status.NextChange = next
	return status
}

// Status evaluates the live settings at the current instant.
func (s *Service) Status() Status { return s.StatusAt(s.nowValue()) }

// IsQuiet reports whether Quiet hours are active now. It is the one read for
// the reply gain and the #62 Notification gate.
func (s *Service) IsQuiet() bool { return s.Status().Quiet }

// AllowDelivery reports whether a Notification may be delivered now. #62 calls
// this before ntfy delivery; the second result names why it is muted. No ntfy
// client is built here.
func (s *Service) AllowDelivery() (bool, string) {
	status := s.Status()
	if status.Quiet {
		switch status.Reason {
		case ReasonDND:
			return false, "do not disturb"
		case ReasonWindow:
			return false, "quiet hours"
		default:
			return false, "quiet hours"
		}
	}
	return true, ""
}

// Gain returns the software speech gain percent: the quiet volume while Quiet
// hours are active, otherwise 100. It never changes the media/sink volume.
func (s *Service) Gain() int {
	status := s.Status()
	if status.Quiet {
		return status.Volume
	}
	return NormalVolume
}

// Speaker is the speech seam a Reminder or reply wrapper uses.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// Speak says text at the quiet gain. #54 uses this for a Reminder's spoken
// message when it lands; replies use the same speech.WithGain seam inside the
// reply session. The sink volume is untouched.
func (s *Service) Speak(ctx context.Context, speaker Speaker, text string) error {
	if speaker == nil {
		return nil
	}
	return speaker.SayText(speech.WithGain(ctx, s.Gain()), text)
}

// Clip scales a Cue to the quiet volume when Quiet hours are active. A Timer
// Ring applies the same rule through RingSettings; #54 uses Clip for the
// Reminder Cue before it speaks the message.
func (s *Service) Clip(clip sound.Clip) sound.Clip {
	gain := s.Gain()
	if gain >= NormalVolume {
		return clip
	}
	return scalePCM(clip, gain)
}

// RingSettings returns the effective Ring settings for one kind. A Timer Ring
// uses the quiet volume as an absolute value inside Quiet hours; an Alarm is
// always unchanged. It is called at Ring start, so a Settings change applies to
// the next Ring, and an overdue Timer recovered at startup already uses the
// saved Quiet override.
func (s *Service) RingSettings(kind ring.Kind, normal ring.Settings) ring.Settings {
	status := s.Status()
	if kind != ring.Timer || !status.Quiet {
		return normal
	}
	normal.Volume = status.Volume
	return normal
}

// RingSpeechGain returns the gain for a Ring's spoken label. Alarms are exempt.
func (s *Service) RingSpeechGain(kind ring.Kind) int {
	if kind == ring.Alarm {
		return NormalVolume
	}
	return s.Gain()
}

// scalePCM scales mono PCM16 samples by volume percent. An unexpected layout is
// returned unchanged rather than reinterpreted.
func scalePCM(clip sound.Clip, volume int) sound.Clip {
	if len(clip.PCM) == 0 || clip.Format.Width != 2 || clip.Format.Channels != 1 {
		return clip
	}
	if volume >= NormalVolume {
		return clip
	}
	gain := float64(volume) / NormalVolume
	samples := len(clip.PCM) / 2
	out := make([]byte, len(clip.PCM))
	for i := 0; i < samples; i++ {
		raw := int16(uint16(clip.PCM[i*2]) | uint16(clip.PCM[i*2+1])<<8)
		scaled := float64(raw) * gain
		if scaled > 32767 {
			scaled = 32767
		}
		if scaled < -32768 {
			scaled = -32768
		}
		value := uint16(int16(scaled))
		out[i*2] = byte(value)
		out[i*2+1] = byte(value >> 8)
	}
	clip.PCM = out
	return clip
}

// HumanClock renders a normalised clock string as a friendly 12-hour time.
func HumanClock(value string) string {
	minutes, ok := clockMinutes(value)
	if !ok {
		return value
	}
	hour := minutes / 60
	minute := minutes % 60
	suffix := "am"
	switch {
	case hour == 0:
		hour = 12
	case hour == 12:
		suffix = "pm"
	case hour > 12:
		hour -= 12
		suffix = "pm"
	}
	return fmt.Sprintf("%d:%02d %s", hour, minute, suffix)
}
