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
	GeocodingURL string          `yaml:"geocoding_url"`
	ForecastURL  string          `yaml:"forecast_url"`
	Timeout      config.Duration `yaml:"timeout"`
	Budget       config.Duration `yaml:"budget"`
}

// DefaultConfig is the shipped upstream and the #35 latency bounds.
func DefaultConfig() Config {
	return Config{
		GeocodingURL: DefaultGeocodingURL,
		ForecastURL:  DefaultForecastURL,
		Timeout:      config.Duration(defaultHTTPTimeout),
		Budget:       config.Duration(defaultBudget),
	}
}

func (c Config) validate() error {
	if strings.TrimSpace(c.GeocodingURL) == "" {
		return errors.New("geocoding_url is required")
	}
	if strings.TrimSpace(c.ForecastURL) == "" {
		return errors.New("forecast_url is required")
	}
	if c.Timeout.Duration() <= 0 {
		return errors.New("timeout must be positive")
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
}

// NewModule builds the weather module with its code defaults.
func NewModule() *Module { return &Module{cfg: DefaultConfig()} }

// Name is the module name and config section suffix.
func (*Module) Name() string { return "weather" }

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
	return Register(r, m.deps())
}

// Health checks that the configured upstreams answer for the current place.
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
	place, err := geocode(ctx, deps, city)
	if err != nil {
		return err
	}
	_, err = forecast(ctx, deps, place.Latitude, place.Longitude)
	return err
}

func (m *Module) deps() Deps {
	return Deps{
		HTTP:         &http.Client{Timeout: m.cfg.Timeout.Duration()},
		GeocodingURL: m.cfg.GeocodingURL,
		ForecastURL:  m.cfg.ForecastURL,
		Timeout:      m.cfg.Timeout.Duration(),
		Budget:       m.cfg.Budget.Duration(),
		Location:     m.location,
	}
}
