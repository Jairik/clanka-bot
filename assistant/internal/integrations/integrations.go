// Package integrations is the small account registry behind the Dashboard's
// Hermes page. It does not add a second credential store: every account is a
// set of the same write-only secrets the Tool modules already resolve, and a
// per-account grant is an ordinary live setting.
//
// The registry is deliberately metadata only. It says which accounts exist,
// which secrets each one needs, and which Tools the account unlocks; the
// secrets service remains the only place a value lives.
package integrations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// GrantPrefix is the settings-key prefix for one account's Hermes grant.
const GrantPrefix = "hermes.integrations."

// GrantKey is the live boolean one account's Hermes access is stored under.
func GrantKey(id string) string { return GrantPrefix + id }

// Status is the derived account state shown on the page.
type Status string

const (
	// StatusLinked means every required secret has a stored value.
	StatusLinked Status = "linked"
	// StatusEnvironment means the required values come from the environment.
	StatusEnvironment Status = "environment"
	// StatusPartial means some required values are present and some are not.
	StatusPartial Status = "partial"
	// StatusMissing means no required value is present.
	StatusMissing Status = "missing"
)

// Account is one external account the owner can link.
type Account struct {
	ID       string
	Name     string
	Summary  string
	Secrets  []string
	Optional []string
	// Tools names the voice Tools this account unlocks. An empty list means the
	// account is Dashboard-only and has no Hermes surface.
	Tools []string
	// Docs is one short sentence the page shows under the account.
	Docs string
}

// Catalog is the fixed account list. Order is page order.
var Catalog = []Account{
	{
		ID:      "github",
		Name:    "GitHub",
		Summary: "Issues, pull requests and notifications on your own repositories.",
		Secrets: []string{"GITHUB_TOKEN"},
		Optional: []string{
			"GITHUB_NOTIFICATIONS_TOKEN",
		},
		Tools: []string{"github.issue_create", "github.pr_list", "github.notifications"},
		Docs:  "Use a fine-grained token limited to the repositories and read/write scopes you want Hermes to have.",
	},
	{
		ID:      "openrouter",
		Name:    "Agent model",
		Summary: "The OpenAI-compatible provider behind the Agent path and the Persona phrase generator.",
		Secrets: []string{"OPENROUTER_API_KEY"},
		Tools:   []string{"agent path", "phrase book generation"},
		Docs:    "Without this key the Fast path still works; Agent requests answer with the Unavailable message.",
	},
	{
		ID:      "spotify",
		Name:    "Spotify",
		Summary: "Playback, playlists and the current track through the Spotify Web API.",
		Secrets: []string{"SPOTIFY_REFRESH_TOKEN"},
		Tools: []string{
			"spotify.play", "spotify.pause", "spotify.next", "spotify.previous",
			"spotify.volume", "spotify.now_playing", "spotify.play_playlist",
			"spotify.shuffle", "spotify.repeat", "spotify.queue_add", "spotify.like",
		},
		Docs: "The client ID is configuration; the refresh token is written when you finish the Spotify sign-in on the Spotify page.",
	},
	{
		ID:      "notify",
		Name:    "ntfy phone push",
		Summary: "Reminders and alerts pushed to the ntfy app on your phone.",
		Secrets: []string{"NTFY_TOPIC"},
		Optional: []string{
			"NTFY_TOKEN",
		},
		Tools: []string{"notify.phone"},
		Docs:  "The topic is a secret so the public ntfy server never shows it in a setting row.",
	},
	{
		ID:      "weather",
		Name:    "WeatherAPI",
		Summary: "Current conditions and forecasts for the weather Tool.",
		Secrets: []string{"WEATHERAPI_API_KEY"},
		Tools:   []string{"weather.current"},
		Docs:    "An optional key; without it the weather Tool reports that it is not configured.",
	},
	{
		ID:      "monster",
		Name:    "Monster Smart Lighting",
		Summary: "The cloud account behind the Monster lights page.",
		Secrets: []string{"MONSTER_EMAIL", "MONSTER_PASSWORD"},
		Docs:    "Dashboard-only today: the Monster module has no voice Tools, so there is no Hermes surface to grant.",
	},
}

// Find returns one catalog account by id.
func Find(id string) (Account, bool) {
	for _, account := range Catalog {
		if account.ID == id {
			return account, true
		}
	}
	return Account{}, false
}

// Definitions declares one live grant per account. They are ordinary settings,
// so the Settings page shows them and a reset revokes the grant.
func Definitions() []settings.Definition {
	out := make([]settings.Definition, 0, len(Catalog))
	for _, account := range Catalog {
		desc := fmt.Sprintf("Allow Hermes to use the %s account. This stores intent only; Hermes can use an account once it is installed and the MCP adapter serves the matching Tools.", account.Name)
		out = append(out, settings.Bool(GrantKey(account.ID), false, settings.WithDescription(desc)))
	}
	return out
}

// Service reads account status from the one secrets service and writes grants
// through the one settings service.
type Service struct {
	secrets  *secrets.Service
	settings *settings.Service
}

// New builds the registry. Either service may be nil; the page then shows
// explanatory rows rather than failing.
func New(secretSvc *secrets.Service, settingsSvc *settings.Service) *Service {
	return &Service{secrets: secretSvc, settings: settingsSvc}
}

// SecretView is one secret's write-only metadata.
type SecretView struct {
	Name     string
	Set      bool
	Source   string
	Last4    string
	Optional bool
}

// View is one account as the page renders it.
type View struct {
	ID       string
	Name     string
	Summary  string
	Docs     string
	Status   Status
	Secrets  []SecretView
	Tools    []string
	Hermes   bool
	Linkable bool
	// CanGrant is false for a Dashboard-only account with no Tool surface.
	CanGrant bool
}

// List returns every account with its derived status and grant.
func (s *Service) List() []View {
	out := make([]View, 0, len(Catalog))
	for _, account := range Catalog {
		out = append(out, s.view(account))
	}
	return out
}

// Get returns one account view.
func (s *Service) Get(id string) (View, bool) {
	account, ok := Find(id)
	if !ok {
		return View{}, false
	}
	return s.view(account), true
}

func (s *Service) view(account Account) View {
	view := View{
		ID:       account.ID,
		Name:     account.Name,
		Summary:  account.Summary,
		Docs:     account.Docs,
		Tools:    append([]string(nil), account.Tools...),
		Hermes:   s.grant(account.ID),
		Linkable: s.secrets != nil,
		CanGrant: len(account.Tools) > 0,
	}
	required := make(map[string]bool, len(account.Secrets))
	for _, name := range account.Secrets {
		required[name] = true
	}
	optional := make(map[string]bool, len(account.Optional))
	for _, name := range account.Optional {
		optional[name] = true
	}
	names := append(append([]string(nil), account.Secrets...), account.Optional...)
	present, envOnly := 0, 0
	for _, name := range names {
		item := SecretView{Name: name, Optional: optional[name]}
		if s.secrets != nil {
			if meta, ok := s.secrets.Metadata(name); ok {
				item.Set = meta.Set
				item.Source = string(meta.Source)
				item.Last4 = meta.Last4
				if meta.Set && required[name] {
					present++
					if meta.Source == secrets.SourceEnv {
						envOnly++
					}
				}
			}
		}
		view.Secrets = append(view.Secrets, item)
	}
	switch total := len(account.Secrets); {
	case total == 0:
		view.Status = StatusMissing
	case present == 0:
		view.Status = StatusMissing
	case present < total:
		view.Status = StatusPartial
	case envOnly == total:
		view.Status = StatusEnvironment
	default:
		view.Status = StatusLinked
	}
	return view
}

func (s *Service) grant(id string) bool {
	if s.settings == nil {
		return false
	}
	return s.settings.Bool(GrantKey(id))
}

// Link stores the supplied values for one account. Unknown secret names and
// missing required values are refused before anything is written, so a partial
// account is an explicit state rather than a surprise.
func (s *Service) Link(ctx context.Context, id string, values map[string]string) error {
	account, ok := Find(id)
	if !ok {
		return fmt.Errorf("integrations: unknown account %q", id)
	}
	if s.secrets == nil {
		return errors.New("integrations: the secrets service is not available")
	}
	allowed := make(map[string]bool, len(account.Secrets)+len(account.Optional))
	for _, name := range append(append([]string(nil), account.Secrets...), account.Optional...) {
		allowed[name] = true
	}
	for name := range values {
		if !allowed[name] {
			return fmt.Errorf("integrations: %s does not belong to %s", name, account.Name)
		}
	}
	for _, name := range account.Secrets {
		if strings.TrimSpace(values[name]) == "" {
			return fmt.Errorf("integrations: %s is required for %s", name, account.Name)
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := strings.TrimSpace(values[name])
		if value == "" {
			continue
		}
		if _, err := s.secrets.Set(ctx, name, value); err != nil {
			return fmt.Errorf("integrations: save %s: %w", name, err)
		}
	}
	return nil
}

// Unlink clears every secret the account owns. Optional values are cleared
// too, so "unlink" means the account is gone rather than half present.
func (s *Service) Unlink(ctx context.Context, id string) error {
	account, ok := Find(id)
	if !ok {
		return fmt.Errorf("integrations: unknown account %q", id)
	}
	if s.secrets == nil {
		return errors.New("integrations: the secrets service is not available")
	}
	for _, name := range append(append([]string(nil), account.Secrets...), account.Optional...) {
		if _, ok := s.secrets.Metadata(name); !ok {
			continue
		}
		if _, err := s.secrets.Clear(ctx, name); err != nil {
			return fmt.Errorf("integrations: clear %s: %w", name, err)
		}
	}
	return nil
}

// SetGrant writes one account's Hermes grant.
func (s *Service) SetGrant(ctx context.Context, id string, allowed bool) error {
	if _, ok := Find(id); !ok {
		return fmt.Errorf("integrations: unknown account %q", id)
	}
	if s.settings == nil {
		return errors.New("integrations: the settings service is not available")
	}
	_, err := s.settings.Set(ctx, GrantKey(id), allowed)
	return err
}

// Granted returns the ids with an active grant, in catalog order.
func (s *Service) Granted() []string {
	if s.settings == nil {
		return nil
	}
	out := make([]string, 0, len(Catalog))
	for _, account := range Catalog {
		if s.settings.Bool(GrantKey(account.ID)) {
			out = append(out, account.ID)
		}
	}
	return out
}
