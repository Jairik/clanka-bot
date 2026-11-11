// Package tools wires every Tool domain into the one registry.
package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
)

// Settings injects the side effects the prototype Tools drive.
type Settings struct {
	Now        func() time.Time // nil uses the real clock
	Audio      audiocontrol.Controls
	TimerAlert func(ctx context.Context, text string) error
	Logger     *slog.Logger
}

// RegisterAll adds every Tool.
func RegisterAll(r *tool.Registry, s Settings) error {
	if err := clock.Register(r, s.Now); err != nil {
		return err
	}
	if err := clock.RegisterDate(r, s.Now); err != nil {
		return err
	}
	if err := audiocontrol.Register(r, s.Audio); err != nil {
		return err
	}
	return timer.Register(r, timer.NewManager(s.Now, s.TimerAlert, s.Logger))
}
