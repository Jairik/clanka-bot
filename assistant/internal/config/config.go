// Package config loads the assistant's non-secret settings from YAML, with a
// few environment overrides for deployment.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tz"
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
	Dashboard Dashboard `yaml:"dashboard"`
	Data      struct {
		// Dir holds the one SQLite database (ADR-0003). Compose mounts the
		// named volume here; local runs use ./data.
		Dir string `yaml:"dir"`
	} `yaml:"data"`
	// Timezone is the IANA zone that clock-time Alarms and Reminders use. It is
	// the config layer of the live `timezone` setting; empty falls back to TZ,
	// then UTC. Nothing is inferred from an IP address.
	Timezone string `yaml:"timezone"`
	Tools    struct {
		DefaultTimeout Duration `yaml:"default_timeout"`
		// Disabled is the config layer for a Tool's enabled setting. Names are
		// canonical Tool names such as weather.current.
		Disabled []string `yaml:"disabled"`
		// Permissions is the config layer for Tool Permission levels. An entry
		// may only tighten the code-declared level.
		Permissions map[string]string `yaml:"permissions"`
		// Sections holds each Tool module's tools.<name> block verbatim; the
		// module decodes its own typed config over its code defaults.
		Sections map[string]yaml.Node `yaml:",inline"`
	} `yaml:"tools"`
	TTS struct {
		Addr    string   `yaml:"addr"` // empty disables spoken replies
		Voice   string   `yaml:"voice"`
		Timeout Duration `yaml:"timeout"`
	} `yaml:"tts"`
	Capture struct {
		// Source names a Pulse source; empty uses the host default.
		Source   string `yaml:"source"`
		Binary   string `yaml:"binary"`
		Rate     int    `yaml:"rate"`
		Width    int    `yaml:"width"`
		Channels int    `yaml:"channels"`
		FrameMS  int    `yaml:"frame_ms"`
	} `yaml:"capture"`
	Wakeword struct {
		Addr    string   `yaml:"addr"` // empty disables voice input
		Model   string   `yaml:"model"`
		Timeout Duration `yaml:"timeout"`
		Chime   struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"chime"`
	} `yaml:"wakeword"`
	STT struct {
		Addr     string   `yaml:"addr"`
		Model    string   `yaml:"model"` // small-streaming or tiny-streaming
		Language string   `yaml:"language"`
		Timeout  Duration `yaml:"timeout"`
		Endpoint struct {
			Silence       Duration `yaml:"silence"`
			ThresholdDBFS float64  `yaml:"threshold_dbfs"`
			MinSpeech     Duration `yaml:"min_speech"`
			MaxUtterance  Duration `yaml:"max_utterance"`
		} `yaml:"endpoint"`
	} `yaml:"stt"`
	Playback struct {
		Binary string `yaml:"binary"`
		Sink   string `yaml:"sink"` // empty uses the Pulse default sink
	} `yaml:"playback"`
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
	Location Location `yaml:"location"`
	Agent    Agent    `yaml:"agent"`
}

// Dashboard is the Tailscale-only web UI listener. Owner empty keeps it off.
type Dashboard struct {
	// Owner is the only Tailscale login allowed in. Empty leaves the Dashboard
	// disabled, so a fresh checkout serves nothing on this listener.
	Owner string `yaml:"owner"`
	// Addr must be loopback or all interfaces. Compose binds all interfaces in
	// the container and publishes only the host loopback port (docs/deploy.md).
	Addr string `yaml:"addr"`
	// HealthTimeout bounds one Health page refresh, including network checks.
	HealthTimeout Duration `yaml:"health_timeout"`
}

// Enabled reports whether an owner is configured.
func (d Dashboard) Enabled() bool { return strings.TrimSpace(d.Owner) != "" }

// Location is the public-IP geolocation settings and the optional fallback
// used when a lookup fails and nothing better is known.
type Location struct {
	ProviderURL     string           `yaml:"provider_url"`
	RefreshInterval Duration         `yaml:"refresh_interval"`
	Timeout         Duration         `yaml:"timeout"`
	Fallback        LocationFallback `yaml:"fallback"`
}

// LocationFallback is the config-supplied place, the last step before no
// location at all.
type LocationFallback struct {
	City     string `yaml:"city"`
	Region   string `yaml:"region"`
	Timezone string `yaml:"timezone"`
}

// Agent is the Agent path's non-secret settings. Secrets live in the env var
// named by Provider.APIKeyEnv.
type Agent struct {
	Provider           string              `yaml:"provider"`
	Providers          map[string]Provider `yaml:"providers"`
	SystemPrompt       string              `yaml:"system_prompt"`
	Filler             string              `yaml:"filler"`
	UnavailableMessage string              `yaml:"unavailable_message"`
	RequestPrefixes    []string            `yaml:"request_prefixes"`
	History            AgentHistory        `yaml:"history"`
	FirstTokenTimeout  Duration            `yaml:"first_token_timeout"`
	RequestTimeout     Duration            `yaml:"request_timeout"`
	MaxSteps           int                 `yaml:"max_steps"`
}

// Provider is one OpenAI-compatible provider entry.
type Provider struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
	// AlternateModel records the written second choice for a manual model
	// switch. It is never read at runtime: there is no failover (ADR-0002).
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
	// The Dashboard stays disabled (owner empty) until the Pi's Tailscale
	// login is configured; the loopback address is where tailscale serve
	// proxies from.
	cfg.Dashboard.Addr = "127.0.0.1:8082"
	cfg.Dashboard.HealthTimeout = Duration(3 * time.Second)
	cfg.Data.Dir = "data"
	cfg.Tools.DefaultTimeout = Duration(2 * time.Second)
	cfg.Embed.URL = "http://embed:8085"
	cfg.Embed.Model = "sentence-transformers/all-MiniLM-L6-v2"
	cfg.Embed.Timeout = Duration(150 * time.Millisecond)
	cfg.Semantic.Threshold = 0.70
	cfg.Semantic.Margin = 0.08
	cfg.Semantic.Joiners = []string{"and", "then", "also"}
	cfg.TTS.Voice = "alba"
	cfg.TTS.Timeout = Duration(30 * time.Second)
	cfg.Playback.Binary = "pacat"
	cfg.Capture.Binary = "parec"
	cfg.Capture.Rate = 16000
	cfg.Capture.Width = 2
	cfg.Capture.Channels = 1
	cfg.Capture.FrameMS = 32
	cfg.Wakeword.Model = "okay_nabu"
	cfg.Wakeword.Timeout = Duration(10 * time.Second)
	cfg.Wakeword.Chime.Enabled = true
	cfg.STT.Model = "small-streaming"
	cfg.STT.Language = "en"
	cfg.STT.Timeout = Duration(20 * time.Second)
	cfg.STT.Endpoint.Silence = Duration(500 * time.Millisecond)
	cfg.STT.Endpoint.ThresholdDBFS = -38
	cfg.STT.Endpoint.MinSpeech = Duration(200 * time.Millisecond)
	cfg.STT.Endpoint.MaxUtterance = Duration(15 * time.Second)
	cfg.Location.ProviderURL = "https://ipwho.is/?fields=success,city,region,timezone"
	cfg.Location.RefreshInterval = Duration(24 * time.Hour)
	cfg.Location.Timeout = Duration(5 * time.Second)
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
		Filler:             "One moment.",
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
	if err := cfg.Validate(); err != nil {
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
	if v := os.Getenv("ASSISTANT_DASHBOARD_OWNER"); v != "" {
		cfg.Dashboard.Owner = v
	}
	if v := os.Getenv("ASSISTANT_DASHBOARD_ADDR"); v != "" {
		cfg.Dashboard.Addr = v
	}
	if v := os.Getenv("ASSISTANT_DATA_DIR"); v != "" {
		cfg.Data.Dir = v
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
	if v := os.Getenv("ASSISTANT_TTS_ADDR"); v != "" {
		cfg.TTS.Addr = v
	}
	if v := os.Getenv("ASSISTANT_TTS_VOICE"); v != "" {
		cfg.TTS.Voice = v
	}
	if v := os.Getenv("ASSISTANT_PULSE_SINK"); v != "" {
		cfg.Playback.Sink = v
	}
	if v := os.Getenv("ASSISTANT_PULSE_SOURCE"); v != "" {
		cfg.Capture.Source = v
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_ADDR"); v != "" {
		cfg.Wakeword.Addr = v
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_MODEL"); v != "" {
		cfg.Wakeword.Model = v
	}
	if v := os.Getenv("ASSISTANT_STT_ADDR"); v != "" {
		cfg.STT.Addr = v
	}
	if v := os.Getenv("ASSISTANT_STT_MODEL"); v != "" {
		cfg.STT.Model = v
	}
	if v := os.Getenv("ASSISTANT_STT_LANGUAGE"); v != "" {
		cfg.STT.Language = v
	}
	return nil
}

// Validate re-checks a loaded Config. Load calls it after applying config.yaml
// and the environment; main calls it again after resolving connection-level
// stored overrides, so a restart cannot start on an invalid combination.
func (c Config) Validate() error {
	if c.HTTP.Addr == "" {
		return fmt.Errorf("config: http.addr is required")
	}
	if c.Debug.Enabled && c.Debug.Addr == "" {
		return fmt.Errorf("config: debug.addr is required when the debug entry is enabled")
	}
	if c.Dashboard.Enabled() {
		if err := validateDashboardAddr(c.Dashboard.Addr); err != nil {
			return err
		}
		if c.Dashboard.HealthTimeout.Duration() <= 0 {
			return fmt.Errorf("config: dashboard.health_timeout must be positive")
		}
		if strings.ContainsAny(c.Dashboard.Owner, " \t\r\n,") {
			return fmt.Errorf("config: dashboard.owner must be one Tailscale login without spaces")
		}
	}
	if strings.TrimSpace(c.Data.Dir) == "" {
		return fmt.Errorf("config: data.dir is required")
	}
	if _, err := tz.Load(c.Timezone); err != nil {
		return fmt.Errorf("config: timezone: %w", err)
	}
	if c.Tools.DefaultTimeout.Duration() <= 0 {
		return fmt.Errorf("config: tools.default_timeout must be positive")
	}
	for _, name := range c.Tools.Disabled {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("config: tools.disabled must not contain an empty Tool name")
		}
	}
	for name, level := range c.Tools.Permissions {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("config: tools.permissions must not contain an empty Tool name")
		}
		switch level {
		case "read", "act", "confirm":
		default:
			return fmt.Errorf("config: tools.permissions.%s must be read, act or confirm, got %q", name, level)
		}
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
	if c.TTS.Addr != "" {
		if c.TTS.Timeout.Duration() <= 0 {
			return fmt.Errorf("config: tts.timeout must be positive")
		}
		if c.TTS.Voice == "" {
			return fmt.Errorf("config: tts.voice is required when tts.addr is set")
		}
	}
	if err := c.validateVoice(); err != nil {
		return err
	}
	if c.Location.RefreshInterval.Duration() <= 0 {
		return fmt.Errorf("config: location.refresh_interval must be positive")
	}
	if c.Location.Timeout.Duration() <= 0 {
		return fmt.Errorf("config: location.timeout must be positive")
	}
	if _, err := c.Agent.ActiveProvider(); err != nil {
		return err
	}
	if c.Agent.UnavailableMessage == "" {
		return fmt.Errorf("config: agent.unavailable_message is required")
	}
	if strings.TrimSpace(c.Agent.Filler) == "" {
		return fmt.Errorf("config: agent.filler is required")
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

// VoiceEnabled reports whether microphone capture is configured. An empty
// wakeword address leaves the assistant typed/debug only.
func (c Config) VoiceEnabled() bool { return c.Wakeword.Addr != "" }

// ValidateDashboardAddr enforces the Dashboard listener rule: loopback, or all
// interfaces so Docker can publish to the host loopback. A stored override is
// held to the same rule as config.yaml, so it cannot widen the listener.
func ValidateDashboardAddr(addr string) error { return validateDashboardAddr(addr) }

// validateDashboardAddr keeps the listener on loopback. The all-interfaces
// form is allowed only because the container must bind that way for Docker's
// published port; the host publication and the handler's source-address check
// are what keep it off the LAN (the debug entry uses the same topology).
func validateDashboardAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("config: dashboard.addr is required when the Dashboard is enabled")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: dashboard.addr must be host:port: %w", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("config: dashboard.addr has an invalid port %q", port)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("config: dashboard.addr must be loopback or all interfaces, got %q", host)
}

func (c Config) validateVoice() error {
	if !c.VoiceEnabled() {
		return nil
	}
	if c.STT.Addr == "" {
		return fmt.Errorf("config: stt.addr is required when wakeword.addr is set")
	}
	if c.Capture.Rate <= 0 || c.Capture.Width != 2 || c.Capture.Channels != 1 || c.Capture.FrameMS <= 0 {
		return fmt.Errorf("config: capture must be 16-bit mono PCM with a positive rate and frame_ms")
	}
	if c.Wakeword.Timeout.Duration() <= 0 {
		return fmt.Errorf("config: wakeword.timeout must be positive")
	}
	if c.STT.Model != "small-streaming" && c.STT.Model != "tiny-streaming" {
		return fmt.Errorf("config: stt.model must be small-streaming or tiny-streaming, got %q", c.STT.Model)
	}
	if c.STT.Language == "" {
		return fmt.Errorf("config: stt.language is required")
	}
	if c.STT.Timeout.Duration() <= 0 {
		return fmt.Errorf("config: stt.timeout must be positive")
	}
	if c.STT.Endpoint.Silence.Duration() <= 0 {
		return fmt.Errorf("config: stt.endpoint.silence must be positive")
	}
	if c.STT.Endpoint.ThresholdDBFS >= 0 {
		return fmt.Errorf("config: stt.endpoint.threshold_dbfs must be below full scale")
	}
	if c.STT.Endpoint.MinSpeech.Duration() < 0 {
		return fmt.Errorf("config: stt.endpoint.min_speech must not be negative")
	}
	if c.STT.Endpoint.MaxUtterance.Duration() < c.STT.Endpoint.Silence.Duration() {
		return fmt.Errorf("config: stt.endpoint.max_utterance must be at least the silence window")
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
