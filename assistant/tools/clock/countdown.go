package clock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
)

// CountdownName is the canonical date.countdown Tool name.
const CountdownName = "date.countdown"

// countdownEvent is one fixed calendar event. Fixed dates only: a floating
// holiday would need its own rule and is not promised here.
type countdownEvent struct {
	Label string
	Month time.Month
	Day   int
}

// countdownEvents is the closed event table. An unknown event stays on the
// Agent path instead of guessing a date.
var countdownEvents = map[string]countdownEvent{
	"christmas":      {"Christmas", time.December, 25},
	"christmas day":  {"Christmas", time.December, 25},
	"xmas":           {"Christmas", time.December, 25},
	"new year":       {"New Year's Day", time.January, 1},
	"new years":      {"New Year's Day", time.January, 1},
	"new years day":  {"New Year's Day", time.January, 1},
	"halloween":      {"Halloween", time.October, 31},
	"valentine":      {"Valentine's Day", time.February, 14},
	"valentines":     {"Valentine's Day", time.February, 14},
	"valentines day": {"Valentine's Day", time.February, 14},
}

// CountdownIn names the event to count down to.
type CountdownIn struct {
	Event string `json:"event"`
}

// CountdownOut is the number of calendar days from today to the next date. It
// is not a duration: DST changes do not add or remove a day.
type CountdownOut struct {
	Event string `json:"event"`
	Days  int    `json:"days"`
	Date  string `json:"date"`
}

// NewCountdown builds date.countdown. A nil now uses time.Now; a nil zone uses
// UTC, matching the Deps contract. A live tz.Service reads the timezone setting
// on every call.
func NewCountdown(now func() time.Time, zone *tz.Service) tool.Tool[CountdownIn, CountdownOut] {
	if now == nil {
		now = time.Now
	}
	return tool.Tool[CountdownIn, CountdownOut]{
		Name:        CountdownName,
		Description: "Count the calendar days until a fixed date such as Christmas, New Year, Halloween or Valentine's Day.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in CountdownIn) (CountdownOut, error) {
			event, ok := findEvent(in.Event)
			if !ok {
				return CountdownOut{}, &tool.ToolError{Say: "I can count down to Christmas, New Year, Halloween or Valentine's Day."}
			}
			location := time.UTC
			if zone != nil {
				location = zone.Location()
			}
			local := now().In(location)
			days, target := countdown(local, event)
			return CountdownOut{Event: event.Label, Days: days, Date: target.Format("2006-01-02")}, nil
		},
		Speak: func(out CountdownOut) string {
			switch out.Days {
			case 0:
				return out.Event + " is today."
			case 1:
				return out.Event + " is tomorrow."
			default:
				return fmt.Sprintf("There are %d days until %s.", out.Days, out.Event)
			}
		},
		Extract: extractCountdown,
		Examples: []string{
			"how many days until Christmas",
			"how many days till Christmas",
			"days until Christmas",
			"how many days until Halloween",
			"how many days until New Year",
			"countdown to Christmas",
			"how many days before Valentine's Day",
			"how many days until New Year's Day",
			"countdown to New Year",
			"countdown to Valentine's Day",
		},
	}
}

// RegisterCountdown adds date.countdown to the registry.
func RegisterCountdown(r *tool.Registry, now func() time.Time, zone *tz.Service) error {
	return tool.Add(r, NewCountdown(now, zone))
}

// countdown returns the calendar-day difference and the target date. It uses
// date-only UTC ordinals, so a DST change between now and the target cannot
// turn 271 calendar days into 270.95 durations rounded the wrong way.
func countdown(local time.Time, event countdownEvent) (int, time.Time) {
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	target := time.Date(local.Year(), event.Month, event.Day, 0, 0, 0, 0, time.UTC)
	if target.Before(today) {
		target = time.Date(local.Year()+1, event.Month, event.Day, 0, 0, 0, 0, time.UTC)
	}
	days := int(target.Sub(today) / (24 * time.Hour))
	return days, target
}

// findEvent normalises a spoken event name and looks it up.
func findEvent(raw string) (countdownEvent, bool) {
	event, ok := countdownEvents[normalizeEvent(raw)]
	return event, ok
}

// normalizeEvent lowercases, removes punctuation and collapses spaces.
func normalizeEvent(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "’", "'")
	name = strings.ReplaceAll(name, "'", "")
	name = strings.ReplaceAll(name, "  ", " ")
	name = strings.Trim(name, " ?.!,;:")
	name = strings.ReplaceAll(name, "  ", " ")
	return strings.TrimSpace(name)
}

// extractCountdown accepts a countdown-shaped question with a known event. It
// leaves "what day is Christmas this year" and unsupported events such as a
// birthday on the Agent path.
func extractCountdown(text string) (CountdownIn, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if !strings.Contains(lower, "day") && !strings.Contains(lower, "countdown") {
		return CountdownIn{}, false
	}
	for _, marker := range []string{"countdown to ", "countdown until ", " days until ", " days till ", " days to ", " until ", " till ", " before "} {
		index := strings.LastIndex(lower, marker)
		if index < 0 {
			continue
		}
		eventText := strings.TrimSpace(text[index+len(marker):])
		if _, ok := findEvent(eventText); ok {
			return CountdownIn{Event: eventText}, true
		}
	}
	return CountdownIn{}, false
}
