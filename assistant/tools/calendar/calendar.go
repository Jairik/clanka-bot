// Package calendar provides calendar.today and calendar.next from a private ICS feed.
package calendar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"gopkg.in/yaml.v3"
)

// Canonical Tool names.
const (
	Name     = "calendar.today"
	NextName = "calendar.next"
)

// SecretName is the write-only secret holding the private iCal feed URL.
const SecretName = "CALENDAR_ICS_URL"

// Config is the tools.calendar section.
type Config struct {
	// AllowAgent allows the Agent path to call calendar tools and see event titles.
	// Defaults to false (LocalOnlyYes) because event titles are personal data.
	AllowAgent bool `yaml:"allow_agent"`
}

// DefaultConfig returns the shipped calendar configuration.
func DefaultConfig() Config {
	return Config{
		AllowAgent: false,
	}
}

func (c Config) validate() error {
	return nil
}

// Module is the calendar Tool domain.
type Module struct {
	cfg     Config
	client  *Client
	secrets tool.SecretResolver
	zone    *tz.Service
	logger  *slog.Logger
	now     func() time.Time
}

// NewModule builds the calendar module with code defaults.
func NewModule() *Module {
	return &Module{
		cfg: DefaultConfig(),
	}
}

// Name is the module name and config section suffix.
func (*Module) Name() string { return "calendar" }

// RequiredSecrets declares CALENDAR_ICS_URL so Dashboard > Secrets and .env can provide it.
func (*Module) RequiredSecrets() []string {
	return []string{SecretName}
}

// Configure decodes tools.calendar over the code defaults.
func (m *Module) Configure(section *yaml.Node) error {
	cfg := DefaultConfig()
	if section != nil {
		if err := section.Decode(&cfg); err != nil {
			return err
		}
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// Register adds calendar.today and calendar.next to the registry.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.logger = deps.Logger
	if m.logger == nil {
		m.logger = slog.Default()
	}
	m.secrets = deps.Secrets
	m.zone = deps.Zone
	m.now = deps.Clock
	m.client = newClient(m.logger, m.now)

	localOnly := tool.LocalOnlyYes
	if m.cfg.AllowAgent {
		localOnly = tool.LocalOnlyNo
	}

	if err := tool.Add(r, m.newToday(localOnly)); err != nil {
		return err
	}
	return tool.Add(r, m.newNext(localOnly))
}

// Health checks if CALENDAR_ICS_URL is configured and reports feed health.
func (m *Module) Health(ctx context.Context) error {
	if m.secrets == nil {
		return errors.New("CALENDAR_ICS_URL is not configured; add it in Dashboard > Secrets or .env")
	}
	url, ok := m.secrets.Get(SecretName)
	url = strings.TrimSpace(url)
	if !ok || url == "" {
		return errors.New("CALENDAR_ICS_URL is not configured; add it in Dashboard > Secrets or .env")
	}

	loc := m.timezone()
	now := m.clock()
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	windowEnd := windowStart.AddDate(0, 0, 8)

	_, err := m.client.Fetch(ctx, url, loc, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("calendar fetch failed (%s)", m.client.ErrorClass())
	}
	if m.client.LastError() != nil {
		return fmt.Errorf("calendar fetch failed (%s)", m.client.ErrorClass())
	}
	return nil
}

// HealthInfo reports a snapshot of the feed's last fetch status.
func (m *Module) HealthInfo() HealthInfo {
	if m.client == nil {
		return HealthInfo{}
	}
	return m.client.HealthInfo()
}

// clock returns the module's time source.
func (m *Module) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// timezone returns the effective timezone.
func (m *Module) timezone() *time.Location {
	if m.zone != nil {
		return m.zone.Location()
	}
	return time.UTC
}

// Event is one serializable event returned to callers.
type Event struct {
	Summary string `json:"summary"`
	Start   string `json:"start"`
	End     string `json:"end"`
	AllDay  bool   `json:"all_day"`
}

// TodayIn is the input for calendar.today.
type TodayIn struct {
	Day string `json:"day,omitempty" jsonschema:"the day to query (e.g. today, tomorrow, or a weekday name); defaults to today"`
}

// TodayOut is the result for calendar.today.
type TodayOut struct {
	Day    string  `json:"day"`
	Events []Event `json:"events"`
	Spoken string  `json:"spoken"`
}

// NextIn is the input for calendar.next (no arguments).
type NextIn struct{}

// NextOut is the result for calendar.next.
type NextOut struct {
	Event  *Event `json:"event,omitempty"`
	Spoken string `json:"spoken"`
}

// newToday constructs the calendar.today Tool.
func (m *Module) newToday(localOnly tool.LocalOnly) tool.Tool[TodayIn, TodayOut] {
	return tool.Tool[TodayIn, TodayOut]{
		Name:        Name,
		Description: "List calendar events for today or a specific day.",
		Permission:  tool.PermissionRead,
		LocalOnly:   localOnly,
		Handler: func(ctx context.Context, in TodayIn) (TodayOut, error) {
			if m.secrets == nil {
				return TodayOut{}, &tool.ToolError{Say: "Your calendar feed URL is not configured."}
			}
			url, ok := m.secrets.Get(SecretName)
			url = strings.TrimSpace(url)
			if !ok || url == "" {
				return TodayOut{}, &tool.ToolError{Say: "Your calendar feed URL is not configured."}
			}

			loc := m.timezone()
			now := m.clock().In(loc)
			day := normalizeDay(in.Day)
			targetDate := resolveDayDate(day, now, loc)

			windowStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
			windowEnd := windowStart.AddDate(0, 0, 8)

			allEvents, err := m.client.Fetch(ctx, url, loc, windowStart, windowEnd)
			if err != nil {
				return TodayOut{}, &tool.ToolError{Say: "I couldn't read your calendar right now."}
			}

			dayStart := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, loc)
			dayEnd := dayStart.AddDate(0, 0, 1)

			var dayEvents []EventInstance
			for _, ev := range allEvents {
				if ev.Start.Before(dayEnd) && ev.End.After(dayStart) {
					dayEvents = append(dayEvents, ev)
				}
			}

			eventsOut := make([]Event, len(dayEvents))
			for i, ev := range dayEvents {
				eventsOut[i] = Event{
					Summary: ev.Summary,
					Start:   ev.Start.In(loc).Format(time.RFC3339),
					End:     ev.End.In(loc).Format(time.RFC3339),
					AllDay:  ev.AllDay,
				}
			}

			spoken := formatTodaySpoken(day, dayEvents, loc)
			return TodayOut{
				Day:    day,
				Events: eventsOut,
				Spoken: spoken,
			}, nil
		},
		Speak: func(out TodayOut) string {
			return out.Spoken
		},
		Examples: []string{
			"what's on my calendar today",
			"what is on my calendar today",
			"what do I have on my calendar today",
			"check my calendar today",
			"what events do I have on my calendar today",
			"tell me my calendar events today",
			"do I have any calendar events today",
		},
		Extract: extractToday,
	}
}

// newNext constructs the calendar.next Tool.
func (m *Module) newNext(localOnly tool.LocalOnly) tool.Tool[NextIn, NextOut] {
	return tool.Tool[NextIn, NextOut]{
		Name:        NextName,
		Description: "Find the next upcoming calendar event.",
		Permission:  tool.PermissionRead,
		LocalOnly:   localOnly,
		Handler: func(ctx context.Context, in NextIn) (NextOut, error) {
			if m.secrets == nil {
				return NextOut{}, &tool.ToolError{Say: "Your calendar feed URL is not configured."}
			}
			url, ok := m.secrets.Get(SecretName)
			url = strings.TrimSpace(url)
			if !ok || url == "" {
				return NextOut{}, &tool.ToolError{Say: "Your calendar feed URL is not configured."}
			}

			loc := m.timezone()
			now := m.clock().In(loc)
			windowStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
			windowEnd := windowStart.AddDate(0, 0, 8)

			allEvents, err := m.client.Fetch(ctx, url, loc, windowStart, windowEnd)
			if err != nil {
				return NextOut{}, &tool.ToolError{Say: "I couldn't read your calendar right now."}
			}

			var nextEv *EventInstance
			for _, ev := range allEvents {
				if !ev.Start.Before(now) {
					nextEv = &ev
					break
				}
			}

			if nextEv == nil {
				return NextOut{
					Spoken: "You have no upcoming events.",
				}, nil
			}

			evOut := &Event{
				Summary: nextEv.Summary,
				Start:   nextEv.Start.In(loc).Format(time.RFC3339),
				End:     nextEv.End.In(loc).Format(time.RFC3339),
				AllDay:  nextEv.AllDay,
			}

			spoken := formatNextSpoken(*nextEv, now, loc)
			return NextOut{
				Event:  evOut,
				Spoken: spoken,
			}, nil
		},
		Speak: func(out NextOut) string {
			return out.Spoken
		},
		Examples: []string{
			"what is my next calendar event",
			"what's my next calendar event",
			"when is my next calendar event",
			"what's the next event on my calendar",
			"what is the next event on my calendar",
			"when is my next appointment on my calendar",
			"what is my next scheduled calendar event",
		},
	}
}

// normalizeDay normalizes the requested day token.
func normalizeDay(day string) string {
	d := strings.ToLower(strings.TrimSpace(day))
	switch d {
	case "", "today":
		return "today"
	case "tomorrow":
		return "tomorrow"
	}
	return d
}

// resolveDayDate determines the date corresponding to day relative to now.
func resolveDayDate(day string, now time.Time, loc *time.Location) time.Time {
	switch day {
	case "today":
		return now
	case "tomorrow":
		return now.AddDate(0, 0, 1)
	}
	for i := 0; i < 7; i++ {
		d := now.AddDate(0, 0, i)
		if strings.EqualFold(d.Weekday().String(), day) {
			return d
		}
	}
	return now
}

// formatTodaySpoken renders natural spoken text for calendar.today.
func formatTodaySpoken(day string, events []EventInstance, loc *time.Location) string {
	dayLabel := "today"
	switch day {
	case "today":
		dayLabel = "today"
	case "tomorrow":
		dayLabel = "tomorrow"
	default:
		dayLabel = "on " + capitalize(day)
	}

	if len(events) == 0 {
		return fmt.Sprintf("You have no events %s.", dayLabel)
	}

	var parts []string
	for _, ev := range events {
		parts = append(parts, formatEventSummary(ev, loc))
	}
	joined := strings.Join(parts, ", ")

	if len(events) == 1 {
		return fmt.Sprintf("You have 1 event %s: %s.", dayLabel, joined)
	}
	return fmt.Sprintf("You have %d events %s: %s.", len(events), dayLabel, joined)
}

// formatNextSpoken renders natural spoken text for calendar.next.
func formatNextSpoken(ev EventInstance, now time.Time, loc *time.Location) string {
	startLoc := ev.Start.In(loc)
	sameDay := startLoc.Year() == now.Year() && startLoc.YearDay() == now.YearDay()
	tomorrow := now.AddDate(0, 0, 1)
	isTomorrow := startLoc.Year() == tomorrow.Year() && startLoc.YearDay() == tomorrow.YearDay()

	if sameDay {
		if ev.AllDay {
			return fmt.Sprintf("Your next event is %s all day.", ev.Summary)
		}
		return fmt.Sprintf("Your next event is %s at %s.", ev.Summary, formatClock(startLoc))
	}
	if isTomorrow {
		if ev.AllDay {
			return fmt.Sprintf("Your next event is %s tomorrow all day.", ev.Summary)
		}
		return fmt.Sprintf("Your next event is %s tomorrow at %s.", ev.Summary, formatClock(startLoc))
	}

	dayName := startLoc.Weekday().String()
	if ev.AllDay {
		return fmt.Sprintf("Your next event is %s on %s all day.", ev.Summary, dayName)
	}
	return fmt.Sprintf("Your next event is %s on %s at %s.", ev.Summary, dayName, formatClock(startLoc))
}

// formatEventSummary describes one event with its start time or all day notice.
func formatEventSummary(ev EventInstance, loc *time.Location) string {
	if ev.AllDay {
		return fmt.Sprintf("%s all day", ev.Summary)
	}
	return fmt.Sprintf("%s at %s", ev.Summary, formatClock(ev.Start.In(loc)))
}

// formatClock returns 12-hour formatted time (e.g. "9:30", "12", "4").
func formatClock(t time.Time) string {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	if t.Minute() == 0 {
		return fmt.Sprintf("%d", h)
	}
	return fmt.Sprintf("%d:%02d", h, t.Minute())
}

// extractToday extracts the day parameter from user input for the Fast path.
func extractToday(text string) (TodayIn, bool) {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "tomorrow") {
		return TodayIn{Day: "tomorrow"}, true
	}
	for _, name := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
		if hasWord(lower, name) {
			return TodayIn{Day: name}, true
		}
	}
	return TodayIn{Day: "today"}, true
}

// hasWord reports whether text contains word as a separate token.
func hasWord(text, word string) bool {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	for _, f := range fields {
		if strings.EqualFold(f, word) {
			return true
		}
	}
	return false
}

// capitalize capitalizes the first letter of a string.
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
