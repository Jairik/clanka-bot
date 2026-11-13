package weather

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
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
}

// NewModule builds the weather module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "weather" }

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
	if err := cfg.validate(); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// Register adds weather.current.
func (m *Module) Register(r *tool.Registry, deps tool.Deps) error {
	m.location = deps.Location
	m.secrets = deps.Secrets
	return Register(r, m.deps())
}

// Health checks that the configured provider chain answers for the current
// place. It runs the Tool's own chain, so one upstream outage is only unhealthy
// when no provider can answer.
func (m *Module) Health(ctx context.Context) error {
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
	return err
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
