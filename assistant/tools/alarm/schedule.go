package alarm

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schedule is how an Alarm repeats. Once is the default; the others are the
// spec's daily, weekdays, weekends and chosen-weekday rules.
type Schedule string

const (
	// Once is a one-off Alarm: today at the clock time, or tomorrow when that
	// time has already passed.
	Once Schedule = "once"
	// Daily repeats every local day.
	Daily Schedule = "daily"
	// Weekdays repeats Monday through Friday.
	Weekdays Schedule = "weekdays"
	// Weekends repeats Saturday and Sunday.
	Weekends Schedule = "weekends"
	// Chosen repeats on the weekdays stored in the row's bitmask.
	Chosen Schedule = "chosen"
)

// scheduleSpec is the normalised repeat rule: the kind plus, for Chosen, a
// Sunday-first weekday bitmask.
type scheduleSpec struct {
	schedule Schedule
	days     uint8
}

// allowed reports whether day is part of the repeat rule.
func (s scheduleSpec) allowed(day time.Weekday) bool {
	switch s.schedule {
	case Daily:
		return true
	case Weekdays:
		return day >= time.Monday && day <= time.Friday
	case Weekends:
		return day == time.Saturday || day == time.Sunday
	case Chosen:
		return s.days&(1<<uint(day)) != 0
	default:
		return false
	}
}

// weekdayWords maps the spoken day names and common abbreviations to a
// time.Weekday. The token parser also accepts a trailing plural "s".
var weekdayWords = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

// weekdayName is the canonical spoken name for one day.
var weekdayName = map[time.Weekday]string{
	time.Sunday:    "sunday",
	time.Monday:    "monday",
	time.Tuesday:   "tuesday",
	time.Wednesday: "wednesday",
	time.Thursday:  "thursday",
	time.Friday:    "friday",
	time.Saturday:  "saturday",
}

// weekdayOrder is Monday-first for display and stable examples.
var weekdayOrder = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

// parseScheduleText reads the repeat rule from a spoken request. Explicit day
// names win, then "weekdays", "weekends" and the daily phrases; no repeat word
// means a one-off Alarm.
func parseScheduleText(text string) (Schedule, []string) {
	words := wordsOf(text)
	seen := make(map[time.Weekday]bool)
	for _, word := range words {
		if day, ok := weekdayToken(word); ok {
			seen[day] = true
		}
	}
	if len(seen) > 0 {
		return Chosen, orderedWeekdayNames(seen)
	}
	joined := " " + strings.Join(words, " ") + " "
	switch {
	case strings.Contains(joined, " weekdays ") || strings.Contains(joined, " weekday "):
		return Weekdays, nil
	case strings.Contains(joined, " weekends ") || strings.Contains(joined, " weekend "):
		return Weekends, nil
	case strings.Contains(joined, " daily "),
		strings.Contains(joined, " everyday "),
		strings.Contains(joined, " every day "),
		strings.Contains(joined, " each day "),
		strings.Contains(joined, " every morning "),
		strings.Contains(joined, " each morning "):
		return Daily, nil
	default:
		return Once, nil
	}
}

// normalizeSchedule turns a Tool argument into a scheduleSpec. A non-empty
// weekday list means Chosen; an explicit Chosen without days is rejected.
func normalizeSchedule(schedule string, weekdays []string) (scheduleSpec, []string, error) {
	normalized := strings.ToLower(strings.TrimSpace(schedule))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	if len(weekdays) > 0 {
		if normalized != "" && normalized != string(Chosen) && normalized != string(Once) {
			return scheduleSpec{}, nil, fmt.Errorf("weekdays cannot be combined with a %q schedule", schedule)
		}
		mask, names, err := parseWeekdayList(weekdays)
		if err != nil {
			return scheduleSpec{}, nil, err
		}
		return scheduleSpec{schedule: Chosen, days: mask}, names, nil
	}
	switch normalized {
	case "", string(Once), "one_off":
		return scheduleSpec{schedule: Once}, nil, nil
	case string(Daily), "every_day", "everyday":
		return scheduleSpec{schedule: Daily}, nil, nil
	case string(Weekdays):
		return scheduleSpec{schedule: Weekdays}, nil, nil
	case string(Weekends):
		return scheduleSpec{schedule: Weekends}, nil, nil
	case string(Chosen), "custom":
		return scheduleSpec{}, nil, fmt.Errorf("a chosen-day alarm needs at least one weekday")
	default:
		return scheduleSpec{}, nil, fmt.Errorf("schedule must be once, daily, weekdays, weekends or chosen")
	}
}

// parseWeekdayList validates spoken day names and returns the bitmask plus the
// canonical Monday-first names.
func parseWeekdayList(values []string) (uint8, []string, error) {
	seen := make(map[time.Weekday]bool)
	for _, raw := range values {
		found := false
		for _, word := range wordsOf(raw) {
			if day, ok := weekdayToken(word); ok {
				seen[day] = true
				found = true
			}
		}
		if !found {
			return 0, nil, fmt.Errorf("%q is not a weekday", raw)
		}
	}
	if len(seen) == 0 {
		return 0, nil, fmt.Errorf("a chosen-day alarm needs at least one weekday")
	}
	var mask uint8
	for day := range seen {
		mask |= 1 << uint(day)
	}
	return mask, orderedWeekdayNames(seen), nil
}

// weekdayToken reads one word as a weekday, accepting a plural suffix.
func weekdayToken(word string) (time.Weekday, bool) {
	word = strings.Trim(word, `.,!?;:'"`)
	if day, ok := weekdayWords[word]; ok {
		return day, true
	}
	if strings.HasSuffix(word, "s") {
		if day, ok := weekdayWords[strings.TrimSuffix(word, "s")]; ok {
			return day, true
		}
	}
	return 0, false
}

// orderedWeekdayNames returns the canonical names in Monday-first order.
func orderedWeekdayNames(seen map[time.Weekday]bool) []string {
	names := make([]string, 0, len(seen))
	for _, day := range weekdayOrder {
		if seen[day] {
			names = append(names, weekdayName[day])
		}
	}
	return names
}

// weekdayNames decodes a stored bitmask into Monday-first canonical names.
func weekdayNames(mask uint8) []string {
	seen := make(map[time.Weekday]bool)
	for _, day := range weekdayOrder {
		if mask&(1<<uint(day)) != 0 {
			seen[day] = true
		}
	}
	return orderedWeekdayNames(seen)
}

// wordsOf lowercases text and splits it into letter/digit words.
func wordsOf(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// nextOccurrence returns the first occurrence of the repeat rule strictly
// after the given instant, interpreted in loc. The search spans eight local
// days, which covers every non-empty Chosen mask.
func nextOccurrence(spec scheduleSpec, hour, minute int, loc *time.Location, after time.Time) time.Time {
	local := after.In(loc)
	for offset := 0; offset < 8; offset++ {
		day := local.AddDate(0, 0, offset)
		if !spec.allowed(day.Weekday()) {
			continue
		}
		candidate := resolveWall(day.Year(), day.Month(), day.Day(), hour, minute, loc)
		if candidate.After(after) {
			return candidate
		}
	}
	// A Chosen mask always has a day inside eight; this defensive fallback
	// keeps a malformed recovered row from stopping the scheduler.
	day := local.AddDate(0, 0, 1)
	return resolveWall(day.Year(), day.Month(), day.Day(), hour, minute, loc)
}

// nextOnce returns the one-off occurrence: today at the clock time, tomorrow
// when that time has already passed, or tomorrow when the request said so.
func nextOnce(now time.Time, hour, minute int, loc *time.Location, tomorrow bool) time.Time {
	local := now.In(loc)
	day := local
	if tomorrow {
		day = local.AddDate(0, 0, 1)
	}
	candidate := resolveWall(day.Year(), day.Month(), day.Day(), hour, minute, loc)
	if !tomorrow && !candidate.After(now) {
		nextDay := local.AddDate(0, 0, 1)
		candidate = resolveWall(nextDay.Year(), nextDay.Month(), nextDay.Day(), hour, minute, loc)
	}
	return candidate
}

// resolveWall turns a local wall-clock time into an instant. A normal time and
// a fall-back overlap resolve to the first occurrence. A spring-forward gap
// has no such instant; the post-transition reading is used, so a 02:30 Alarm
// fires at 03:30 when the clock jumps over 02:30. Both rules are deterministic
// and covered by tests.
func resolveWall(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
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
	// which is the post-transition offset, so the Alarm fires after the jump.
	var latest time.Time
	for offset := range offsets {
		instant := nominal.Add(-time.Duration(offset) * time.Second)
		if instant.After(latest) {
			latest = instant
		}
	}
	return latest
}

// formatClock renders a 12-hour spoken clock time, for example "7:00 am".
func formatClock(hour, minute int) string {
	suffix := "am"
	display := hour
	if hour >= 12 {
		suffix = "pm"
	}
	if display > 12 {
		display -= 12
	}
	if display == 0 {
		display = 12
	}
	return fmt.Sprintf("%d:%02d %s", display, minute, suffix)
}

// describeSchedule is the short spoken repeat phrase.
func describeSchedule(spec scheduleSpec, weekdays []string) string {
	switch spec.schedule {
	case Daily:
		return " every day"
	case Weekdays:
		return " on weekdays"
	case Weekends:
		return " on weekends"
	case Chosen:
		return " on " + joinWeekdays(weekdays)
	default:
		return ""
	}
}

// joinWeekdays renders "monday and tuesday" or "monday, wednesday and friday".
func joinWeekdays(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
