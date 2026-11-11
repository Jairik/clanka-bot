// Package clock implements the time.now and date.today Tools.
package clock

import (
	"context"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical time.now Tool name.
const Name = "time.now"

// DateName is the canonical date.today Tool name.
const DateName = "date.today"

// In has no fields: asking the time needs no arguments.
type In struct{}

// Out is the current local time. The RFC 3339 encoding carries the offset.
type Out struct {
	Time time.Time `json:"time"`
}

// New builds the time.now Tool. A nil now uses time.Now.
func New(now func() time.Time) tool.Tool[In, Out] {
	if now == nil {
		now = time.Now
	}
	return tool.Tool[In, Out]{
		Name:        Name,
		Description: "Get the current local time.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, In) (Out, error) {
			return Out{Time: now()}, nil
		},
		Speak: func(out Out) string {
			return "It's " + out.Time.Format("3:04 PM") + "."
		},
		Examples: []string{
			"what time is it",
			"what's the time",
			"tell me the time",
		},
	}
}

// Register adds time.now to the registry.
func Register(r *tool.Registry, now func() time.Time) error {
	return tool.Add(r, New(now))
}

// RegisterDate adds date.today to the registry.
func RegisterDate(r *tool.Registry, now func() time.Time) error {
	return tool.Add(r, NewDate(now))
}

// DateOut is the current local date.
type DateOut struct {
	Date time.Time `json:"date"`
}

// NewDate builds the date.today Tool. A nil now uses time.Now.
func NewDate(now func() time.Time) tool.Tool[In, DateOut] {
	if now == nil {
		now = time.Now
	}
	return tool.Tool[In, DateOut]{
		Name:        DateName,
		Description: "Get today's local date.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, In) (DateOut, error) {
			return DateOut{Date: now()}, nil
		},
		Speak: func(out DateOut) string {
			return "Today is " + out.Date.Format("Monday, 2 January 2006") + "."
		},
		Examples: []string{
			"what's the date",
			"what is today's date",
			"what day is it",
			"what's today's date",
		},
	}
}
