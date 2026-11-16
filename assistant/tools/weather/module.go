package weather

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/httpclient"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// Config is the tools.weather section.
type Config struct {
	WeatherAPIURL string          `yaml:"weatherapi_url"`
	GeocodingURL  string          `yaml:"geocoding_url"`
	ForecastURL   string          `yaml:"forecast_url"`
	FallbackURL   string          `yaml:"fallback_url"`
	FallbackDelay config.Duration `yaml:"fallback_delay"`
	Budget        config.Duration `yaml:"budget"`
}

// DefaultConfig is the shipped provider order and the #35 latency bounds.
func DefaultConfig() Config {
	return Config{
		WeatherAPIURL: DefaultWeatherAPIURL,
		GeocodingURL:  DefaultGeocodingURL,
		ForecastURL:   DefaultForecastURL,
		FallbackURL:   DefaultFallbackURL,
		FallbackDelay: config.Duration(defaultFallbackDelay),
		Budget:        config.Duration(defaultBudget),
	}
}

func (c Config) validate() error {
	if strings.TrimSpace(c.WeatherAPIURL) == "" {
		return errors.New("weatherapi_url is required")
	}
	// The WeatherAPI key travels in the query string, so there is no insecure
	// opt-in for this one. The keyless Open-Meteo and wttr.in URLs stay free.
	if err := httpclient.RequireSecureOrigin(c.WeatherAPIURL, false); err != nil {
		return fmt.Errorf("weatherapi_url: %w", err)
	}
	if strings.TrimSpace(c.GeocodingURL) == "" {
		return errors.New("geocoding_url is required")
	}
	if strings.TrimSpace(c.ForecastURL) == "" {
		return errors.New("forecast_url is required")
	}
	if strings.TrimSpace(c.FallbackURL) == "" {
		return errors.New("fallback_url is required")
	}
	if c.FallbackDelay.Duration() <= 0 {
		return errors.New("fallback_delay must be positive")
	}
	if c.Budget.Duration() <= 0 {
		return errors.New("budget must be positive")
	}
	return nil
}

// Module is the weather Tool domain.
type Module struct {
	cfg      Config
	location func() (location.Location, bool)
	secrets  tool.SecretResolver
	// settings returns the one live settings service; the Tool reads the
	// temperature unit from it per reply. Nil means the code default applies.
	settings func() *settings.Service
	// now is the module's clock for health ages; nil uses time.Now.
	now    func() time.Time
	health healthMemo
}

// NewModule builds the weather module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "weather" }

// WeatherAPIURL is the configured current.json URL, used by the Dashboard
// secrets verifier so it probes the same endpoint the Tool uses.
func (m *Module) WeatherAPIURL() string { return m.cfg.WeatherAPIURL }

// RequiredSecrets declares the optional WeatherAPI key so the write-only store
// can hold it and the Dashboard Secrets page lists it. Without it, Open-Meteo
// and wttr.in still answer.
func (*Module) RequiredSecrets() []string { return []string{SecretName} }

// Configure decodes tools.weather over the code defaults.
func (m *Module) Configure(section *yaml.Node) error {
	cfg := DefaultConfig()
	if section != nil {
		if err := section.Decode(&cfg); err != nil {
			return err
		}
	}
	// Store the same credential-bearing URL that the origin validator checks.
	cfg.WeatherAPIURL = strings.TrimSpace(cfg.WeatherAPIURL)
	if err := cfg.validate(); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// Register adds weather.current. A successful live answer also seeds the Health
// memo, so the Health row does not pay a second provider round trip right after
// a spoken or typed weather request.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.location = deps.Location
	m.secrets = deps.Secrets
	m.settings = deps.Settings
	def := New(m.deps())
	inner := def.Handler
	def.Handler = func(ctx context.Context, in In) (Out, error) {
		out, err := inner(ctx, in)
		// Health checks the current place, so only a call for that place can
		// stand in for it; a named-city answer says nothing about home.
		if err == nil && strings.TrimSpace(in.City) == "" {
			m.health.record(m.clock(), nil)
		}
		return out, err
	}
	return tool.Add(r, def)
}

// Health checks that the configured provider chain answers for the current
// place. It runs the Tool's own chain, so one upstream outage is only unhealthy
// when no provider can answer. The answer is memoised like the GitHub token
// check: a cold provider chain can lose the shared snapshot's 3 s race even when
// weather still answers for every real request (#208).
func (m *Module) Health(ctx context.Context) error {
	if err, ok := m.health.fresh(m.clock()); ok {
		return err
	}
	deps := m.deps()
	city, err := resolveCity(In{}, deps)
	if err != nil {
		return err
	}
	if deps.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.Budget)
		defer cancel()
	}
	_, err = fetchCurrent(ctx, deps, newCache(), city)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// The snapshot ran out of time; that says nothing about the providers,
		// so leave the memo alone and let the next refresh retry.
		return err
	}
	m.health.record(m.clock(), err)
	return err
}

// clock is the module's time source for health ages. Production leaves the
// field nil; tests inject one to exercise the TTL without waiting.
func (m *Module) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Module) deps() Deps {
	return Deps{
		HTTP:          &http.Client{Timeout: m.cfg.Budget.Duration()},
		APIKey:        m.apiKey,
		WeatherAPIURL: m.cfg.WeatherAPIURL,
		GeocodingURL:  m.cfg.GeocodingURL,
		ForecastURL:   m.cfg.ForecastURL,
		FallbackURL:   m.cfg.FallbackURL,
		FallbackDelay: m.cfg.FallbackDelay.Duration(),
		Budget:        m.cfg.Budget.Duration(),
		Location:      m.location,
		Settings:      m.settings,
	}
}

// apiKey reads the declared secret on every request, so a Dashboard rotation
// applies without rebuilding the module. The stored value wins over .env.
func (m *Module) apiKey() (string, bool) {
	if m.secrets == nil {
		return "", false
	}
	value, ok := m.secrets.Get(SecretName)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

const (
	// healthOKTTL is how long a successful provider-chain answer is reused.
	healthOKTTL = 5 * time.Minute
	// healthFailTTL keeps an outage visible without paying the whole chain on
	// every refresh.
	healthFailTTL = 30 * time.Second
)

// healthMemo is the last Health answer. The zero value is empty, so a module
// that was never checked runs a live check on first use.
type healthMemo struct {
	mu   sync.Mutex
	set  bool
	at   time.Time
	err  error
	fail bool
}

// fresh returns the remembered answer while it is younger than its TTL.
func (m *healthMemo) fresh(now time.Time) (error, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.set {
		return nil, false
	}
	ttl := healthOKTTL
	if m.fail {
		ttl = healthFailTTL
	}
	if now.Sub(m.at) >= ttl {
		return nil, false
	}
	return m.err, true
}

// record stores one answer and whether it was a failure.
func (m *healthMemo) record(now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set, m.at, m.err, m.fail = true, now, err, err != nil
}
