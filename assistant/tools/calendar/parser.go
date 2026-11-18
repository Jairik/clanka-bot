// Package calendar provides calendar.today and calendar.next from a private ICS feed.
package calendar

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// EventInstance represents one concrete event occurrence.
type EventInstance struct {
	Summary string
	Start   time.Time
	End     time.Time
	AllDay  bool
}

// parseICS parses VEVENT components from an iCalendar stream, expands RRULE
// recurrences, and returns all occurrences overlapping [windowStart, windowEnd).
func parseICS(r io.Reader, loc *time.Location, windowStart, windowEnd time.Time) ([]EventInstance, error) {
	if loc == nil {
		loc = time.UTC
	}
	dec := ical.NewDecoder(r)
	cal, err := dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to decode iCalendar: %w", err)
	}

	// Detached instances suppress their original occurrence even when cancelled or moved outside the window.
	overrides := make(map[string][]time.Time)
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		uid, _ := child.Props.Text("UID")
		if prop := child.Props.Get("RECURRENCE-ID"); uid != "" && prop != nil {
			if original, err := parseEventTime(prop, loc); err == nil {
				overrides[uid] = append(overrides[uid], original)
			}
		}
	}

	var events []EventInstance
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		// Cancelled detached instances have already suppressed their master occurrence.
		if status := child.Props.Get("STATUS"); status != nil && strings.EqualFold(status.Value, "CANCELLED") {
			continue
		}
		summary, _ := child.Props.Text(ical.PropSummary)
		startProp := child.Props.Get(ical.PropDateTimeStart)
		if startProp == nil {
			continue
		}

		allDay := isAllDayProp(startProp)
		startTime, err := parseEventTime(startProp, loc)
		if err != nil {
			continue
		}

		endProp := child.Props.Get(ical.PropDateTimeEnd)
		var endTime time.Time
		var duration time.Duration
		var days int
		var remainder time.Duration
		if endProp != nil {
			endTime, err = parseEventTime(endProp, loc)
			if err != nil {
				endTime = startTime
			}
			if !endTime.Before(startTime) {
				duration = endTime.Sub(startTime)
				if allDay && isAllDayProp(endProp) {
					// Date values describe calendar days even across a DST change.
					days = int(utcDate(endTime).Sub(utcDate(startTime)) / (24 * time.Hour))
				}
			}
		} else if durProp := child.Props.Get(ical.PropDuration); durProp != nil {
			duration = parseICSDuration(durProp.Value)
			endTime = startTime.Add(duration)
			if allDay {
				days = int(duration / (24 * time.Hour))
				remainder = duration % (24 * time.Hour)
				endTime = startTime.AddDate(0, 0, days).Add(remainder)
			}
		} else if allDay {
			duration = 24 * time.Hour
			days = 1
			endTime = startTime.AddDate(0, 0, 1)
		} else {
			duration = 0
			endTime = startTime
		}

		rruleProp := child.Props.Get(ical.PropRecurrenceRule)
		if rruleProp != nil {
			roption, err := rrule.StrToROption(rruleProp.Value)
			if err == nil && roption != nil {
				roption.Dtstart = startTime
				rule, err := rrule.NewRRule(*roption)
				if err == nil {
					set := rrule.Set{}
					set.RRule(rule)
					for _, ex := range parseExDates(child.Props, loc) {
						set.ExDate(ex)
					}
					uid, _ := child.Props.Text("UID")
					for _, original := range overrides[uid] {
						set.ExDate(original)
					}
					// Search back by the event duration to catch overlapping recurrences.
					expandStart := windowStart.Add(-duration)
					if days > 0 {
						expandStart = windowStart.Add(-remainder).In(startTime.Location()).AddDate(0, 0, -days)
					}
					occs := set.Between(expandStart, windowEnd, true)
					for _, occ := range occs {
						occEnd := occ.Add(duration)
						if days > 0 {
							occEnd = occ.AddDate(0, 0, days).Add(remainder)
						}
						if occ.Before(windowEnd) && occEnd.After(windowStart) {
							events = append(events, EventInstance{
								Summary: summary,
								Start:   occ,
								End:     occEnd,
								AllDay:  allDay,
							})
						}
					}
					continue
				}
			}
		}

		// Non-recurring event
		if startTime.Before(windowEnd) && endTime.After(windowStart) {
			events = append(events, EventInstance{
				Summary: summary,
				Start:   startTime,
				End:     endTime,
				AllDay:  allDay,
			})
		}
	}

	sort.Slice(events, func(i, j int) bool {
		if events[i].Start.Equal(events[j].Start) {
			return events[i].Summary < events[j].Summary
		}
		return events[i].Start.Before(events[j].Start)
	})

	return events, nil
}

// utcDate preserves the displayed calendar date without a time zone offset.
func utcDate(instant time.Time) time.Time {
	year, month, day := instant.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// isAllDayProp reports whether a DTSTART property indicates an all-day event.
func isAllDayProp(prop *ical.Prop) bool {
	if prop == nil {
		return false
	}
	if strings.EqualFold(prop.Params.Get("VALUE"), "DATE") {
		return true
	}
	val := strings.TrimSpace(prop.Value)
	return len(val) == 8 && !strings.Contains(val, "T")
}

// parseEventTime parses a date or date-time property value respecting TZID.
func parseEventTime(prop *ical.Prop, defaultLoc *time.Location) (time.Time, error) {
	if prop == nil {
		return time.Time{}, fmt.Errorf("nil property")
	}
	if defaultLoc == nil {
		defaultLoc = time.UTC
	}
	tzid := strings.Trim(prop.Params.Get(ical.PropTimezoneID), "\"")
	targetLoc := defaultLoc
	if tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			targetLoc = l
		}
	}
	val := strings.TrimSpace(prop.Value)
	return parseTimeString(val, targetLoc)
}

// parseTimeString parses RFC 5545 date and datetime strings into time.Time.
func parseTimeString(s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	s = strings.TrimSpace(s)
	// Format: YYYYMMDDTHHMMSSZ (UTC)
	if strings.HasSuffix(s, "Z") {
		return time.Parse("20060102T150405Z", s)
	}
	// Format: YYYYMMDDTHHMMSS (local in loc)
	if strings.Contains(s, "T") {
		return time.ParseInLocation("20060102T150405", s, loc)
	}
	// Format: YYYYMMDD (all-day date in loc)
	if len(s) == 8 {
		return time.ParseInLocation("20060102", s, loc)
	}
	return time.Time{}, fmt.Errorf("unrecognized time format %q", s)
}

// parseExDates extracts and parses all EXDATE values from property list.
func parseExDates(props ical.Props, defaultLoc *time.Location) []time.Time {
	var result []time.Time
	for _, prop := range props.Values("EXDATE") {
		tzid := strings.Trim(prop.Params.Get("TZID"), "\"")
		targetLoc := defaultLoc
		if tzid != "" {
			if l, err := time.LoadLocation(tzid); err == nil {
				targetLoc = l
			}
		}
		for _, part := range strings.Split(prop.Value, ",") {
			part = strings.TrimSpace(part)
			if t, err := parseTimeString(part, targetLoc); err == nil {
				result = append(result, t)
			}
		}
	}
	return result
}

// parseICSDuration parses basic ISO 8601 durations like P1D, PT1H, PT30M.
func parseICSDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "P") {
		return 0
	}
	s = s[1:]
	var d time.Duration
	if idx := strings.Index(s, "T"); idx >= 0 {
		datePart := s[:idx]
		timePart := s[idx+1:]
		if strings.HasSuffix(datePart, "D") {
			days, _ := strconv.Atoi(datePart[:len(datePart)-1])
			d += time.Duration(days) * 24 * time.Hour
		}
		var num int
		for i := 0; i < len(timePart); i++ {
			c := timePart[i]
			if c >= '0' && c <= '9' {
				num = num*10 + int(c-'0')
			} else if c == 'H' {
				d += time.Duration(num) * time.Hour
				num = 0
			} else if c == 'M' {
				d += time.Duration(num) * time.Minute
				num = 0
			} else if c == 'S' {
				d += time.Duration(num) * time.Second
				num = 0
			}
		}
	} else if strings.HasSuffix(s, "D") {
		days, _ := strconv.Atoi(s[:len(s)-1])
		d += time.Duration(days) * 24 * time.Hour
	}
	return d
}
