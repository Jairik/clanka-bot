// Package reminder implements the reminder.set, reminder.list and
// reminder.cancel Tools. A Reminder is a Scheduled alert that speaks its
// message once instead of ringing. It is persisted in the one SQLite store
// (ADR-0003), recovered at startup with the same two-minute grace and downtime
// summary as a Timer, and played as one urgent Announcement item: the Cue
// immediately before the message, at quiet.volume inside Quiet hours.
package reminder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	quietcore "github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Canonical Tool names.
const (
	SetName    = "reminder.set"
	ListName   = "reminder.list"
	CancelName = "reminder.cancel"
)

// SetIn is one Reminder request. Exactly one of DurationSeconds and At is set.
// At is a local "HH:MM" clock time in the effective timezone; Tomorrow makes
// an explicit "tomorrow" survive the future-if-past policy.
type SetIn struct {
	Message         string `json:"message"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	At              string `json:"at,omitempty"`
	Tomorrow        bool   `json:"tomorrow,omitempty"`
}

// SetOut is the Reminder that was created.
type SetOut = Reminder

// ListIn has no arguments.
type ListIn struct{}

// ListOut holds the active Reminders.
type ListOut struct {
	Reminders []Reminder `json:"reminders"`
}

// CancelIn names a Reminder by id or by a phrase from its message; empty
// cancels every Reminder.
type CancelIn struct {
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
}

// CancelOut reports how many Reminders were cancelled.
type CancelOut struct {
	Cancelled int `json:"cancelled"`
}

// NewSet builds reminder.set. The destination is the Agent path as well as the
// Fast path, so it is LocalOnlyNo; the message itself never leaves the Pi
// unless the user's own Agent request carries it.
func NewSet(m *Manager) tool.Tool[SetIn, SetOut] {
	return tool.Tool[SetIn, SetOut]{
		Name:        SetName,
		Description: "Set a reminder for a duration or a clock time; it speaks the message once when due.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in SetIn) (SetOut, error) {
			switch {
			case in.DurationSeconds > 0:
				r, err := m.StartDuration(in.Message, seconds(in.DurationSeconds))
				return r, toolError(err)
			case strings.TrimSpace(in.At) != "":
				hour, minute, ok := parseClockInput(in.At)
				if !ok {
					return SetOut{}, &tool.ToolError{Say: "I couldn't understand that time."}
				}
				r, err := m.StartClock(in.Message, hour, minute, in.Tomorrow)
				return r, toolError(err)
			default:
				return SetOut{}, &tool.ToolError{Say: "When should I remind you?"}
			}
		},
		Speak: func(out SetOut) string {
			if out.ID == "" {
				return "I couldn't set that reminder."
			}
			return "Okay, I'll remind you to " + out.Message + " " + out.When + "."
		},
		Examples: []string{
			"remind me in 20 minutes to check the oven",
			"remind me at 5 pm to call mum",
			"remind me to take the bins out in ten minutes",
			"set a reminder for 30 minutes to stretch",
			"remind me at 7:30 to leave for work",
			"remind me tomorrow at 9 am to call the dentist",
			"can you remind me to water the plants at 6 pm",
			"remind me in an hour to switch the laundry",
			"remind me at noon to eat lunch",
			"set a reminder for two hours to call the plumber",
			"remind me in 90 seconds to check the rice",
		},
		Extract: func(text string) (SetIn, bool) {
			return parseSetInput(text)
		},
	}
}

// NewList builds reminder.list. It is LocalOnlyYes: the messages can be
// personal, so the Agent path can neither see nor call it.
func NewList(m *Manager) tool.Tool[ListIn, ListOut] {
	return tool.Tool[ListIn, ListOut]{
		Name:        ListName,
		Description: "List the active reminders and when they are due.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, ListIn) (ListOut, error) {
			return ListOut{Reminders: m.List()}, nil
		},
		Speak: func(out ListOut) string {
			switch len(out.Reminders) {
			case 0:
				return "No reminders are set."
			case 1:
				return "Your reminder to " + out.Reminders[0].Message + " is " + out.Reminders[0].When + "."
			default:
				next := out.Reminders[0]
				return fmt.Sprintf("You have %d reminders. The next is to %s %s.",
					len(out.Reminders), next.Message, next.When)
			}
		},
		Examples: []string{
			"what reminders do I have",
			"which reminders are set",
			"show me the reminder list",
			"do I have any reminders set",
			"how many reminders are there",
			"read out my reminders",
			"what's on my reminder list",
			"what reminders are waiting",
			"show me my reminders",
			"which reminders do I have",
			"tell me my reminders",
		},
		Extract: func(text string) (ListIn, bool) {
			// A question about another device's reminder list is not this Tool.
			if mentionsExternalStore(text) {
				return ListIn{}, false
			}
			return ListIn{}, true
		},
	}
}

// NewCancel builds reminder.cancel. The message query is a case-insensitive
// substring, so "cancel the oven reminder" finds "check the oven".
func NewCancel(m *Manager) tool.Tool[CancelIn, CancelOut] {
	return tool.Tool[CancelIn, CancelOut]{
		Name:        CancelName,
		Description: "Cancel one reminder by id or message, or every reminder.",
		Permission:  tool.PermissionAct,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(_ context.Context, in CancelIn) (CancelOut, error) {
			var (
				cancelled int
				err       error
			)
			if strings.TrimSpace(in.ID) != "" {
				cancelled, err = m.Cancel(in.ID)
			} else {
				cancelled, err = m.CancelQuery(in.Message)
			}
			return CancelOut{Cancelled: cancelled}, toolError(err)
		},
		Speak: func(out CancelOut) string {
			if out.Cancelled == 0 {
				return "No reminder to cancel."
			}
			return "Reminder cancelled."
		},
		Examples: []string{
			"cancel the reminder",
			"cancel the oven reminder",
			"delete the dentist reminder",
			"remove the reminder about the bins",
			"forget the laundry reminder",
			"cancel every reminder",
			"get rid of the oven reminder",
			"cancel the reminder to call mum",
			"clear my reminders",
			"cancel the dentist reminder",
		},
		Extract: func(text string) (CancelIn, bool) {
			// The reminder noun is what distinguishes this Tool from
			// timer.cancel and "cancel it"; without it the Agent path decides.
			lower := strings.ToLower(text)
			if !strings.Contains(lower, "reminder") && !strings.Contains(lower, "remind") {
				return CancelIn{}, false
			}
			return CancelIn{Message: cancellationQuery(text)}, true
		},
	}
}

// Register adds every Reminder Tool.
func Register(r *tool.Registry, m *Manager) error {
	if err := tool.Add(r, NewSet(m)); err != nil {
		return err
	}
	if err := tool.Add(r, NewList(m)); err != nil {
		return err
	}
	return tool.Add(r, NewCancel(m))
}

// seconds converts a whole-second Tool input to a duration.
func seconds(value int) time.Duration {
	return time.Duration(value) * time.Second
}

// parseClockInput reads the Tool's canonical "HH:MM" form and the friendly
// forms the Agent may supply ("5 pm", "17:30"), through the same parser Quiet
// hours uses for a clock setting.
func parseClockInput(value string) (int, int, bool) {
	normalized, ok := quietcore.ParseClock(value)
	if !ok {
		return 0, 0, false
	}
	hour := int(normalized[0]-'0')*10 + int(normalized[1]-'0')
	minute := int(normalized[3]-'0')*10 + int(normalized[4]-'0')
	return hour, minute, true
}

// mentionsExternalStore rejects a question about a phone, calendar or app
// reminder list. The Pi's list is the only one this Tool can answer.
func mentionsExternalStore(text string) bool {
	lower := strings.ToLower(text)
	for _, word := range []string{"phone", "iphone", "android", "calendar", "outlook", "app "} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// toolError turns a manager validation error into the spoken Tool error.
func toolError(err error) error {
	if err == nil {
		return nil
	}
	var spoken *Error
	if errors.As(err, &spoken) {
		return &tool.ToolError{Say: spoken.Message}
	}
	return err
}
