package tool

import (
	"context"
	"log/slog"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"gopkg.in/yaml.v3"
)

// Deps are the dependencies shared by every Tool module. The zero value is
// usable: the clock is the real one, the logger is the default, and the
// optional seams are absent.
type Deps struct {
	// Clock is the module's only source of the current time; nil uses time.Now.
	Clock func() time.Time
	// Logger is the module's logger; nil uses slog.Default().
	Logger *slog.Logger
	// Location returns the last known place; nil means the place is unknown.
	Location func() (location.Location, bool)
	// Speaker speaks unprompted and alert text; nil drops it.
	Speaker Speaker
	// Audio drives the speaker volume and playback; nil makes the audio Tools unavailable.
	Audio AudioControls
}

// Speaker is the speech seam a module uses for alert speech.
type Speaker interface {
	SayText(ctx context.Context, text string) error
}

// AudioControls is the speaker-control seam the audio Tools drive.
type AudioControls interface {
	SetVolume(ctx context.Context, percent int) error
	AdjustVolume(ctx context.Context, delta int) error
	Stop(ctx context.Context) error
}

// Module is one Tool domain. Name is also its config section, tools.<name>.
type Module interface {
	Name() string
	Register(r *Registry, deps Deps) error
}

// Configurable is a Module with its own typed config. Configure decodes the
// tools.<name> section over the module's code defaults; a nil section means
// the section is absent.
type Configurable interface {
	Configure(section *yaml.Node) error
}

// HealthChecker is a Module's optional health check.
type HealthChecker interface {
	Health(ctx context.Context) error
}
