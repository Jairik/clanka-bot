package clock

import (
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Module is the clock Tool domain. It has no settings, so tools.clock is not
// a valid config section.
type Module struct{}

// NewModule builds the clock module.
func NewModule() *Module { return &Module{} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "clock" }

// Register adds time.now, date.today, time.in_city and date.countdown.
func (*Module) Register(r *tool.Registry, deps tool.Deps) error {
	now := deps.Clock
	if now == nil {
		now = time.Now
	}
	// time.now and date.today report the live timezone setting, not the
	// container's TZ: the zone is read per call (the setting is live), with
	// TZ then UTC as the fallback chain date.countdown already uses.
	zone := deps.Zone
	zoned := func() time.Time {
		location := time.UTC
		if zone != nil {
			location = zone.Location()
		}
		return now().In(location)
	}
	if err := Register(r, zoned); err != nil {
		return err
	}
	if err := RegisterDate(r, zoned); err != nil {
		return err
	}
	if err := RegisterCity(r, deps.Clock); err != nil {
		return err
	}
	return RegisterCountdown(r, deps.Clock, deps.Zone)
}
