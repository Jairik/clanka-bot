// Package config loads the assistant's non-secret settings from YAML, with a
// few environment overrides for deployment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	Agent          Agent             `yaml:"agent"`
}

// Agent is the Agent path's non-secret settings. Secrets live in the env var
// named by Provider.APIKeyEnv.
type Agent struct {
	Provider           string              `yaml:"provider"`
	Providers          map[string]Provider `yaml:"providers"`
	SystemPrompt       string              `yaml:"system_prompt"`
	UnavailableMessage string              `yaml:"unavailable_message"`
	RequestPrefixes    []string            `yaml:"request_prefixes"`
	History            AgentHistory        `yaml:"history"`
	FirstTokenTimeout  Duration            `yaml:"first_token_timeout"`
	RequestTimeout     Duration            `yaml:"request_timeout"`
	MaxSteps           int                 `yaml:"max_steps"`
}

// Provider is one OpenAI-compatible provider entry.
type Provider struct {
	BaseURL        string            `yaml:"base_url"`
	APIKeyEnv      string            `yaml:"api_key_env"`
	Model          string            `yaml:"model"`
	AlternateModel string            `yaml:"alternate_model"`
	Headers        map[string]string `yaml:"headers"`
	Routing        map[string]any    `yaml:"routing"`
}

// AgentHistory configures the rolling conversation history.
type AgentHistory struct {
	Turns int      `yaml:"turns"`
	Quiet Duration `yaml:"quiet"`
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
	cfg.Agent = DefaultAgent()
	return cfg
}

// DefaultAgent returns the prototype Agent path settings.
func DefaultAgent() Agent {
	return Agent{
		Provider: "openrouter",
		Providers: map[string]Provider{
			"openrouter": {
				BaseURL:        "https://openrouter.ai/api/v1",
				APIKeyEnv:      "OPENROUTER_API_KEY",
				Model:          "deepseek/deepseek-v4.1-flash",
				AlternateModel: "deepseek/deepseek-v4-pro",
				Routing:        map[string]any{"sort": "latency"},
			},
		},
		UnavailableMessage: "Sorry, I can't answer that right now.",
		RequestPrefixes:    []string{"ask the agent", "agent", "hey agent"},
		History: AgentHistory{
			Turns: 10,
			Quiet: Duration(5 * time.Minute),
		},
		FirstTokenTimeout: Duration(8 * time.Second),
		RequestTimeout:    Duration(60 * time.Second),
		MaxSteps:          6,
	}
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
	if _, err := c.Agent.ActiveProvider(); err != nil {
		return err
	}
	if c.Agent.UnavailableMessage == "" {
		return fmt.Errorf("config: agent.unavailable_message is required")
	}
	for _, prefix := range c.Agent.RequestPrefixes {
		if strings.TrimSpace(prefix) == "" {
			return fmt.Errorf("config: agent.request_prefixes must not contain an empty prefix")
		}
	}
	if c.Agent.History.Turns <= 0 {
		return fmt.Errorf("config: agent.history.turns must be positive")
	}
	if c.Agent.History.Quiet.Duration() <= 0 {
		return fmt.Errorf("config: agent.history.quiet must be positive")
	}
	if c.Agent.FirstTokenTimeout.Duration() <= 0 {
		return fmt.Errorf("config: agent.first_token_timeout must be positive")
	}
	if c.Agent.RequestTimeout.Duration() <= 0 {
		return fmt.Errorf("config: agent.request_timeout must be positive")
	}
	if c.Agent.MaxSteps <= 0 {
		return fmt.Errorf("config: agent.max_steps must be positive")
	}
	return nil
}

// ActiveProvider returns the one provider entry the Agent path uses. There is
// no failover, so a missing or incomplete entry is a config error.
func (a Agent) ActiveProvider() (Provider, error) {
	if a.Provider == "" {
		return Provider{}, fmt.Errorf("config: agent.provider is required")
	}
	provider, ok := a.Providers[a.Provider]
	if !ok {
		return Provider{}, fmt.Errorf("config: agent.providers has no entry for %q", a.Provider)
	}
	if provider.BaseURL == "" {
		return Provider{}, fmt.Errorf("config: agent.providers.%s.base_url is required", a.Provider)
	}
	if provider.APIKeyEnv == "" {
		return Provider{}, fmt.Errorf("config: agent.providers.%s.api_key_env is required", a.Provider)
	}
	if provider.Model == "" {
		return Provider{}, fmt.Errorf("config: agent.providers.%s.model is required", a.Provider)
	}
	return provider, nil
}
