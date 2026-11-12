package tool

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
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
	// Cues plays a fixed-role Cue through the shared speaker. #54 plays the
	// Reminder Cue before announcing and #52 loops a Ring Cue; #55 wires the
	// seam but neither consumer exists yet. Nil means no Cue sound.
	Cues *sound.Service
	// Names is the one shared matcher for spoken library names: sounds,
	// voices and Spotify playlists. It reads the live threshold per lookup and
	// never forces a guess; nil means a module cannot resolve spoken names.
	Names *namematch.Matcher
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
