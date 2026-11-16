package alerts

import (
	"fmt"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/parse"
	alarmtypes "github.com/Jairik/clanka-bot/assistant/tools/alarm"
	"github.com/Jairik/clanka-bot/assistant/tools/reminder"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// The source constructors take a lazy manager getter. Module wiring passes a
// module's Manager method (the manager exists only after Register); test and
// eval registries can pass a closure over an already-built manager. A nil
// getter, or one that yields nil, is reported as an unavailable source.

// TimerSource adapts a timer manager's List seam.
func TimerSource(manager func() *timer.Manager) Source { return timerSource{manager: manager} }

type timerSource struct{ manager func() *timer.Manager }

func (s timerSource) NextAlerts() ([]Item, bool) {
	if s.manager == nil || s.manager() == nil {
		return nil, false
	}
	list := s.manager().List()
	if len(list) == 0 {
		return nil, true
	}
	soonest := list[0] // List is soonest first, ringing included.
	return []Item{{Source: "timer", ID: soonest.ID, At: soonest.FiresAt, Text: timerText(soonest)}}, true
}

func timerText(t timer.Timer) string {
	if t.State == timer.StateRinging {
		if t.Label != "" {
			return "your " + t.Label + " timer is ringing"
		}
		return "your timer is ringing"
	}
	left := parse.HumanDuration(time.Duration(t.RemainingSeconds) * time.Second)
	if t.Label != "" {
		return "the " + t.Label + " timer has " + left + " left"
	}
	return "your timer has " + left + " left"
}

// AlarmSource adapts an alarm manager's List seam.
func AlarmSource(manager func() *alarmtypes.Manager) Source { return alarmSource{manager: manager} }

type alarmSource struct{ manager func() *alarmtypes.Manager }

func (s alarmSource) NextAlerts() ([]Item, bool) {
	if s.manager == nil || s.manager() == nil {
		return nil, false
	}
	list := s.manager().List()
	if len(list) == 0 {
		return nil, true
	}
	soonest := list[0]
	return []Item{{Source: "alarm", ID: soonest.ID, At: soonest.NextFiresAt, Text: alarmText(soonest)}}, true
}

func alarmText(a alarmtypes.Alarm) string {
	if a.State == alarmtypes.StateRinging {
		if a.Label != "" {
			return "your " + a.Label + " alarm is ringing"
		}
		return "your alarm is ringing"
	}
	label := ""
	if a.Label != "" {
		label = a.Label + " "
	}
	return "your " + clockText(a.Hour, a.Minute) + " " + label + "alarm is next"
}

// clockText renders a stored 24-hour clock as a spoken 12-hour time.
func clockText(hour, minute int) string {
	period := "am"
	if hour >= 12 {
		period = "pm"
	}
	hour %= 12
	if hour == 0 {
		hour = 12
	}
	return fmt.Sprintf("%d:%02d %s", hour, minute, period)
}

// ReminderSource adapts a reminder manager's List seam.
func ReminderSource(manager func() *reminder.Manager) Source { return reminderSource{manager: manager} }

type reminderSource struct{ manager func() *reminder.Manager }

func (s reminderSource) NextAlerts() ([]Item, bool) {
	if s.manager == nil || s.manager() == nil {
		return nil, false
	}
	list := s.manager().List()
	if len(list) == 0 {
		return nil, true
	}
	soonest := list[0]
	return []Item{{Source: "reminder", ID: soonest.ID, At: soonest.FiresAt, Text: reminderText(soonest)}}, true
}

func reminderText(r reminder.Reminder) string {
	if r.State == reminder.StateRinging {
		return "your reminder to " + r.Message + " is ringing"
	}
	return "a reminder to " + r.Message + " is " + r.When
}
