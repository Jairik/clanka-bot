package tool

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
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
	// Speaker speaks alert text and other direct speech; nil drops it.
	Speaker Speaker
	// Audio drives the speaker volume and playback; nil makes the audio Tools unavailable.
	Audio AudioControls
	// Store is the assistant's one SQLite database (ADR-0003); nil means a
	// module cannot persist state.
	Store *store.Store
	// Secrets resolves a module's declared secrets from the store, then the
	// environment bootstrap. Nil means no secret can be resolved.
	Secrets SecretResolver
	// Announcements queues speech the user did not just ask for. A ringing
	// Timer or Alarm is not an Announcement; nil drops it.
	Announcements *announce.Queue
	// Focus routes shared Control-phrase intents to the holder at each
	// priority. A module that can hold Focus claims here; nil means the module
	// still works but a shared phrase cannot reach it.
	Focus *focus.Registry
	// Zone resolves the live timezone for clock-time Alarms and Reminders.
	// Duration Timers do not need it; nil means UTC.
	Zone *tz.Service
	// Cues plays a fixed-role Cue through the shared speaker. A Timer Ring
	// loops sounds.timer_ring through internal/ring (#52), an Alarm will use
	// sounds.alarm_ring (#53), and #54 plays the Reminder Cue before
	// announcing. Nil means no Cue sound.
	Cues *sound.Service
	// Ring is the shared Scheduled-alert Ring engine. Timer and Alarm attach it
	// before their Start hook recovers persisted rows, so even an immediately
	// overdue alert Rings instead of falling back to the old spoken line. Nil
	// keeps the spoken fallback.
	Ring *ring.Engine
	// Names is the one shared matcher for spoken library names: sounds,
	// voices and Spotify playlists. It reads the live threshold per lookup and
	// never forces a guess; nil means a module cannot resolve spoken names.
	Names *namematch.Matcher
	// Settings returns the live settings service once main has created it. It
	// is a function because Tool modules register before the service exists;
	// a module that needs a live setting reads it per action and must not
	// snapshot it. Nil means only code defaults are available.
	Settings func() *settings.Service
	// Quiet is the live Quiet hours service. The Quiet Tools write through it,
	// and a future Reminder (#54) can use its Speak/Clip helpers so its speech
	// and Cue obey quiet.volume. Nil means Quiet hours are unavailable.
	Quiet *quiet.Service
	// Voices is the persistent voice library and active selection shared with
	// the Dashboard. The voice Tools read and write it; nil registration is
	// allowed so the conformance suite can check metadata without a database.
	Voices *voicelib.Service
	// Notify sends Notifications to the user's phone through ntfy. A module
	// that owns a Scheduled alert installs the missed hook from this seam
	// before recovery, so a recovered missed row is not lost. Nil means no
	// Notification is delivered.
	Notify Notifier
	// LastReply holds the previous completed Assistant reply and its
	// provenance. assistant.repeat speaks it again and notify.phone resolves
	// "that" from it; a Local-only reply must be refused before it leaves the
	// Pi. Nil means there is no reply to refer to.
	LastReply *lastreply.Store
	// Status runs the shared service/module health checks. It is a function
	// because the Tool is registered before the live module list has finished
	// registering; a Tool reads it per call. Nil means status is unavailable.
	Status func(context.Context) StatusReport
}

// Notifier is the shared Notification seam. The production implementation is
// internal/notify.Service; tests may supply a fake.
type Notifier interface {
	Send(ctx context.Context, n notify.Notification) error
	NotifyMissed(ctx context.Context, alert notify.MissedAlert)
}

// StatusState is one health-check outcome. The string values are stable for
// logs and the Dashboard's CSS classes.
type StatusState string

const (
	StatusOK            StatusState = "ok"
	StatusError         StatusState = "error"
	StatusNotConfigured StatusState = "not-configured"
	StatusTimedOut      StatusState = "timed-out"
	StatusNoCheck       StatusState = "no-check"
)

// StatusCheck is one service, location or Tool module health result.
type StatusCheck struct {
	Name   string
	State  StatusState
	Detail string
}

// StatusReport is one bounded snapshot of the shared health boundaries.
type StatusReport struct {
	CheckedAt time.Time
	Timeout   time.Duration
	Services  []StatusCheck
	Location  StatusCheck
	Modules   []StatusCheck
}

// OK reports whether at least one check ran and every check that can fail is
// healthy. A not-configured service and a module without a check are neutral,
// not failures, but an empty snapshot is not proof of health either.
func (r StatusReport) OK() bool {
	checks := make([]StatusCheck, 0, len(r.Services)+len(r.Modules)+1)
	checks = append(checks, r.Services...)
	checks = append(checks, r.Modules...)
	checks = append(checks, r.Location)
	ran := 0
	for _, check := range checks {
		switch check.State {
		case StatusOK, StatusError, StatusTimedOut:
			ran++
		}
		if check.State == StatusError || check.State == StatusTimedOut {
			return false
		}
	}
	return ran > 0
}

// SecretResolver is the shared seam a module uses to read a declared secret.
// The stored value wins over the environment; the second result is false when
// neither layer has one.
type SecretResolver interface {
	Get(name string) (string, bool)
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

// SecretDeclarer is a Module's optional list of secret names it needs. A name
// is both the secrets-table key and the .env bootstrap variable; the stored
// value wins. Modules that need no secret omit this interface.
type SecretDeclarer interface {
	RequiredSecrets() []string
}

// IntentDeclarer is a Module's optional declaration of the shared Focus
// intents it handles when it holds Focus. A module that never takes Focus
// omits this interface. The declaration is static; the holder a module claims
// at runtime declares the same set.
type IntentDeclarer interface {
	Intents() []focus.Intent
}

// Startable is a Module's optional post-registration hook. main calls it after
// the live settings, Cues, Zone and Ring seams are bound, so a module that
// recovers persisted schedules starts them with the effective configuration
// instead of the code defaults. It must run once, after Register and before
// serving; a module without persisted state omits it.
type Startable interface {
	Start(ctx context.Context) error
}

// Closer is a Module's optional shutdown hook. main calls it once after the
// registry stops taking requests, so a module with a scheduler can stop its
// callbacks and finish a durable write before the database closes. Modules
// without background work omit it.
type Closer interface {
	Close()
}

// SecretNames collects every secret declared by the modules, in module order,
// with duplicate names collapsed. Blank declarations are omitted here and
// rejected by the conformance suite.
func SecretNames(modules ...Module) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, module := range modules {
		declarer, ok := module.(SecretDeclarer)
		if !ok {
			continue
		}
		for _, raw := range declarer.RequiredSecrets() {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}
