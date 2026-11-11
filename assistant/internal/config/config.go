// Package config loads the assistant's non-secret settings from YAML, with a
// few environment overrides for deployment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the non-secret runtime configuration.
type Config struct {
	HTTP struct {
		Addr string `yaml:"addr"`
	} `yaml:"http"`
	Debug struct {
		Enabled bool   `yaml:"enabled"`
		Addr    string `yaml:"addr"`
	} `yaml:"debug"`
	Tools struct {
		DefaultTimeout Duration `yaml:"default_timeout"`
	} `yaml:"tools"`
	ControlPhrases map[string]string `yaml:"control_phrases"`
	Embed          struct {
		URL     string   `yaml:"url"`
		Model   string   `yaml:"model"`
		Timeout Duration `yaml:"timeout"`
	} `yaml:"embed"`
	Semantic struct {
		Threshold float64  `yaml:"threshold"`
		Margin    float64  `yaml:"margin"`
		Joiners   []string `yaml:"joiners"`
	} `yaml:"semantic"`
	Agent struct {
		Prefixes []string `yaml:"prefixes"`
	} `yaml:"agent"`
}

// Duration is a YAML-friendly time.Duration written as "2s" or "750ms".
type Duration time.Duration

// Duration returns the standard library duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// Default returns the prototype defaults.
func Default() Config {
	var cfg Config
	cfg.HTTP.Addr = ":8080"
	cfg.Debug.Enabled = true
	cfg.Debug.Addr = "127.0.0.1:8081"
	cfg.Tools.DefaultTimeout = Duration(2 * time.Second)
	cfg.Embed.URL = "http://embed:8085"
	cfg.Embed.Model = "sentence-transformers/all-MiniLM-L6-v2"
	cfg.Embed.Timeout = Duration(150 * time.Millisecond)
	cfg.Semantic.Threshold = 0.70
	cfg.Semantic.Margin = 0.08
	cfg.Semantic.Joiners = []string{"and", "then", "also"}
	cfg.Agent.Prefixes = []string{"ask the agent", "agent", "hey agent"}
	return cfg
}

// Load reads path over the defaults and then applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	if v := os.Getenv("ASSISTANT_HTTP_ADDR"); v != "" {
		cfg.HTTP.Addr = v
	}
	if v := os.Getenv("ASSISTANT_DEBUG_ADDR"); v != "" {
		cfg.Debug.Addr = v
	}
	if v := os.Getenv("ASSISTANT_DEBUG_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_DEBUG_ENABLED=%q: %w", v, err)
		}
		cfg.Debug.Enabled = enabled
	}
	if v := os.Getenv("ASSISTANT_EMBED_URL"); v != "" {
		cfg.Embed.URL = v
	}
	if v := os.Getenv("ASSISTANT_EMBED_MODEL"); v != "" {
		cfg.Embed.Model = v
	}
	if v := os.Getenv("ASSISTANT_SEMANTIC_THRESHOLD"); v != "" {
		threshold, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_SEMANTIC_THRESHOLD=%q: %w", v, err)
		}
		cfg.Semantic.Threshold = threshold
	}
	if v := os.Getenv("ASSISTANT_SEMANTIC_MARGIN"); v != "" {
		margin, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_SEMANTIC_MARGIN=%q: %w", v, err)
		}
		cfg.Semantic.Margin = margin
	}
	return nil
}

func (c Config) validate() error {
	if c.HTTP.Addr == "" {
		return fmt.Errorf("config: http.addr is required")
	}
	if c.Debug.Enabled && c.Debug.Addr == "" {
		return fmt.Errorf("config: debug.addr is required when the debug entry is enabled")
	}
	if c.Tools.DefaultTimeout.Duration() <= 0 {
		return fmt.Errorf("config: tools.default_timeout must be positive")
	}
	if c.Embed.URL == "" {
		return fmt.Errorf("config: embed.url is required")
	}
	if c.Embed.Model == "" {
		return fmt.Errorf("config: embed.model is required")
	}
	if c.Embed.Timeout.Duration() <= 0 {
		return fmt.Errorf("config: embed.timeout must be positive")
	}
	if c.Semantic.Threshold <= 0 || c.Semantic.Threshold > 1 {
		return fmt.Errorf("config: semantic.threshold must be in (0, 1]")
	}
	if c.Semantic.Margin < 0 || c.Semantic.Margin >= 1 {
		return fmt.Errorf("config: semantic.margin must be in [0, 1)")
	}
	return nil
}
