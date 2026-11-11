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
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

// Options carries the injected dependencies used when registering Tools.
type Options struct {
	Now        func() time.Time // nil uses the real clock
	Weather    weather.Deps
	Audio      audiocontrol.Controls
	TimerAlert func(ctx context.Context, text string) error
	Logger     *slog.Logger
}

// RegisterAll adds every Tool. Zero Options use the real clock and the default
// upstream URLs; tests override them.
func RegisterAll(r *tool.Registry, opts Options) error {
	if err := clock.Register(r, opts.Now); err != nil {
		return err
	}
	if err := clock.RegisterDate(r, opts.Now); err != nil {
		return err
	}
	if err := weather.Register(r, opts.Weather); err != nil {
		return err
	}
	if err := audiocontrol.Register(r, opts.Audio); err != nil {
		return err
	}
	return timer.Register(r, timer.NewManager(opts.Now, opts.TimerAlert, opts.Logger))
}
