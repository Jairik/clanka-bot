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
	TTS struct {
		Addr    string   `yaml:"addr"` // empty disables spoken replies
		Voice   string   `yaml:"voice"`
		Timeout Duration `yaml:"timeout"`
	} `yaml:"tts"`
	Playback struct {
		Binary string `yaml:"binary"`
		Sink   string `yaml:"sink"` // empty uses the Pulse default sink
	} `yaml:"playback"`
	ControlPhrases map[string]string `yaml:"control_phrases"`
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
	cfg.TTS.Voice = "alba"
	cfg.TTS.Timeout = Duration(30 * time.Second)
	cfg.Playback.Binary = "pacat"
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
	if v := os.Getenv("ASSISTANT_TTS_ADDR"); v != "" {
		cfg.TTS.Addr = v
	}
	if v := os.Getenv("ASSISTANT_TTS_VOICE"); v != "" {
		cfg.TTS.Voice = v
	}
	if v := os.Getenv("ASSISTANT_PULSE_SINK"); v != "" {
		cfg.Playback.Sink = v
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
	if c.TTS.Addr != "" {
		if c.TTS.Timeout.Duration() <= 0 {
			return fmt.Errorf("config: tts.timeout must be positive")
		}
		if c.TTS.Voice == "" {
			return fmt.Errorf("config: tts.voice is required when tts.addr is set")
		}
	}
	return nil
}
