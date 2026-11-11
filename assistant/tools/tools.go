// Package tools wires every Tool domain into the one registry.
package tools

import (
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// RegisterAll adds every Tool. A nil now uses the real clock.
func RegisterAll(r *tool.Registry, now func() time.Time) error {
	if err := clock.Register(r, now); err != nil {
		return err
	}
	return nil
}
