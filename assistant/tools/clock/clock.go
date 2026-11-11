// Package clock implements the time.now Tool.
package clock

import (
	"context"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Name is the canonical Tool name.
const Name = "time.now"

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
