package alarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// NewSet builds alarm.set.
func NewSet(m *Manager) tool.Tool[SetIn, SetOut] {
	return tool.Tool[SetIn, SetOut]{
		Name:        SetName,
		Description: "Set a clock-time alarm, optionally repeating on chosen days.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in SetIn) (SetOut, error) {
			return m.Set(ctx, in)
		},
		Speak: func(out SetOut) string {
			clock := formatClock(out.Hour, out.Minute)
			repeat := describeSchedule(scheduleSpec{schedule: out.Schedule, days: weekdayMaskOf(out.Weekdays)}, out.Weekdays)
			if out.Label != "" {
				return capitalize(out.Label) + " alarm set for " + clock + repeat + "."
			}
			return "Alarm set for " + clock + repeat + "."
		},
		Examples: []string{
			"wake me at 7",
			"wake me up at 6:30",
			"set an alarm for 7 in the morning",
			"set an alarm for 6:30 on weekdays",
			"set an alarm for 8 on weekends",
			"set an alarm for 7 every day",
			"set an alarm for 6:45 on monday",
			"set a work alarm for 7:30",
		},
		Extract: extractSet,
	}
}

// NewList builds alarm.list.
func NewList(m *Manager) tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the configured alarms and when the next one rings.",
		Permission:  tool.PermissionRead,
		// The list names private labels, so it never leaves the Pi.
		LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, ListIn) (ListOut, error) {
			return ListOut{Alarms: m.List()}, nil
		},
		Speak: func(out ListOut) string {
			switch len(out.Alarms) {
			case 0:
				return "No alarms are set."
			case 1:
				return describeAlarm(out.Alarms[0])
			default:
				next := out.Alarms[0]
				return fmt.Sprintf("You have %d alarms. The next is at %s.",
					len(out.Alarms), formatClock(next.Hour, next.Minute))
			}
		},
		Examples: []string{
			"what alarms do I have",
			"list my alarms",
			"show me my alarms",
			"what alarms are set",
			"do I have any alarms",
			"when is my next alarm",
		},
	}
}

// NewCancel builds alarm.cancel.
func NewCancel(m *Manager) tool.Tool[CancelIn, CancelOut] {
	return tool.Tool[CancelIn, CancelOut]{
		Name:        CancelName,
		Description: "Cancel an alarm by time, label or id; no argument cancels every alarm.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(ctx context.Context, in CancelIn) (CancelOut, error) {
			cancelled, err := m.Cancel(ctx, in)
			return CancelOut{Cancelled: cancelled}, err
		},
		Speak: func(out CancelOut) string {
			switch out.Cancelled {
			case 0:
				return "No alarm to cancel."
			case 1:
				return "Alarm cancelled."
			default:
				return fmt.Sprintf("Cancelled %d alarms.", out.Cancelled)
			}
		},
		Examples: []string{
			"cancel my 7 am alarm",
			"cancel the alarm",
			"cancel my alarm",
			"delete my alarm",
			"remove the 6:30 alarm",
			"cancel my work alarm",
			"cancel my wake up alarm",
		},
		Extract: extractCancel,
	}
}

// Register adds every alarm Tool.
func Register(r *tool.Registry, m *Manager) error {
	if err := tool.Add(r, NewSet(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewList(m)); err != nil {
		return err
	}
	return tool.Add(r, NewCancel(m))
}

// extractSet reads the clock time and repeat rule from a spoken alarm request.
// It requires an alarm or wake phrase, so a misrouted timer request cannot
// create an Alarm.
func extractSet(text string) (SetIn, bool) {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "alarm") && !strings.Contains(lower, "wake") {
		return SetIn{}, false
	}
	hour, minute, ok := parse.Clock(text)
	if !ok {
		return SetIn{}, false
	}
	schedule, weekdays := parseScheduleText(text)
	return SetIn{
		Hour:     hour,
		Minute:   minute,
		Schedule: string(schedule),
		Weekdays: weekdays,
		Label:    labelForAlarm(text),
		Tomorrow: mentionsTomorrow(text),
	}, true
}

// extractCancel reads the alarm selector. A request that does not mention an
// alarm is not an alarm.cancel request, so a misrouted "cancel my subscription"
// cannot cancel every Alarm.
func extractCancel(text string) (CancelIn, bool) {
	if !strings.Contains(strings.ToLower(text), "alarm") {
		return CancelIn{}, false
	}
	in := CancelIn{Label: labelForAlarm(text)}
	if hour, minute, ok := parse.Clock(text); ok {
		in.Time = fmt.Sprintf("%02d:%02d", hour, minute)
	}
	return in, true
}

// mentionsTomorrow reports whether the request names tomorrow explicitly.
func mentionsTomorrow(text string) bool {
	for _, word := range wordsOf(text) {
		if word == "tomorrow" {
			return true
		}
	}
	return false
}

// weekdayMaskOf turns spoken weekday names into the stored bitmask.
func weekdayMaskOf(names []string) uint8 {
	mask, _, err := parseWeekdayList(names)
	if err != nil {
		return 0
	}
	return mask
}

// describeAlarm renders one Alarm for the list reply.
func describeAlarm(alarm Alarm) string {
	repeat := describeSchedule(scheduleSpec{schedule: alarm.Schedule, days: weekdayMaskOf(alarm.Weekdays)}, alarm.Weekdays)
	clock := formatClock(alarm.Hour, alarm.Minute)
	if alarm.Label != "" {
		return capitalize(alarm.Label) + " alarm is set for " + clock + repeat + "."
	}
	return "Your alarm is set for " + clock + repeat + "."
}
