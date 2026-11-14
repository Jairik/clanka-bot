// Package config loads the assistant's non-secret settings from YAML, with a
// few environment overrides for deployment.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/httpguard"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/spotify"
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
	// Spotify is the non-secret Web API app configuration plus the Soloist
	// pairing heartbeat path. The refresh token is a write-only secret.
	Spotify spotify.Config `yaml:"spotify"`
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
	// Voice is the custom voice-state library and the config layer of the live
	// voice.rate setting. voice.name's config layer stays tts.voice.
	Voice Voice `yaml:"voice"`
	// VoiceSamples is the Dashboard recorder's take store and the host-visible
	// bundle export directory. Nothing here is uploaded or served publicly;
	// the owner copies a finished bundle to the desktop by hand.
	VoiceSamples VoiceSamples `yaml:"voice_samples"`
	Capture      struct {
		// Source names a Pulse source; empty uses the host default.
		Source   string `yaml:"source"`
		Binary   string `yaml:"binary"`
		Rate     int    `yaml:"rate"`
		Width    int    `yaml:"width"`
		Channels int    `yaml:"channels"`
		FrameMS  int    `yaml:"frame_ms"`
	} `yaml:"capture"`
	Wakeword Wakeword `yaml:"wakeword"`
	STT      struct {
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
	// Sounds is the config layer for the five fixed Cue roles. Each value is a
	// built-in name, a WAV file in data/sounds, a volume path, or none. Empty
	// leaves the built-in chime; a stored override wins.
	Sounds Sounds `yaml:"sounds"`
	// Quiet is the config layer for the live Quiet hours settings. The daily
	// window is evaluated in the effective timezone; the Dashboard and the
	// quiet.* Tools write the same stored overrides.
	Quiet Quiet `yaml:"quiet"`
	// Notify is the config layer for the live ntfy URL and missed-alert
	// switch. The topic and token are secrets, never config values.
	Notify         Notify            `yaml:"notify"`
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
	// NameMatch tunes the shared spoken-name matcher. Threshold is the config
	// layer of the live name_match.threshold setting.
	NameMatch struct {
		Threshold float64 `yaml:"threshold"`
	} `yaml:"name_match"`
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
	// Hosts is the explicit public Host allowlist: the tailnet name
	// `tailscale serve` exposes. A request with any other Host is refused even
	// when it carries the owner identity, so a DNS-rebinding page cannot reach
	// the Dashboard. Loopback names stay allowed for the local smoke test.
	Hosts []string `yaml:"hosts"`
	// HealthTimeout bounds one Health page refresh, including network checks.
	HealthTimeout Duration `yaml:"health_timeout"`
	// UIURL is the internal base URL of the standalone Dashboard web artifact
	// (the dashboard-web Compose service). Empty means the embedded bundle is
	// the only copy. The assistant proxies through this URL so the browser
	// still sees one origin, and falls back to the embedded bundle when the
	// artifact is stopped.
	UIURL string `yaml:"ui_url"`
	// UIVersion is the artifact pin (the deploy SHA), reported in a response
	// header so an operator can see which UI a page came from.
	UIVersion string `yaml:"ui_version"`
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

// Quiet is the config layer for the daily Quiet hours window and its absolute
// volume. It is the quiet package's typed config, so the YAML layer and the
// settings definitions cannot drift. Start and end are local clock strings such
// as "22:30"; an end earlier than the start crosses midnight. A stored
// Dashboard or voice override wins.
type Quiet = quiet.Config

// Notify is the config layer for the live ntfy URL and missed-alert switch. It
// is the notify package's typed config; the generated topic and any access
// token stay in the write-only secrets table.
type Notify = notify.Config

// Voice configures the custom voice-state library. Files are pre-exported
// Pocket TTS .safetensors states; the folder is mounted read-only into tts and
// never baked into an image.
type Voice struct {
	// Dir holds custom .safetensors voice states. Empty means data.dir/voices.
	// A relative path resolves under data.dir; an absolute path is used as-is,
	// so a volume path can be configured without the Dashboard.
	Dir string `yaml:"dir"`
	// TTSDir is the same folder's path inside the tts container. The assistant
	// sends "<TTSDir>/<file>" with a synthesis request.
	TTSDir string `yaml:"tts_dir"`
	// Rate is the config layer of the live voice.rate multiplier (1.0 normal).
	Rate float64 `yaml:"rate"`
}

// VoiceDir resolves the assistant-side library path.
func (c Config) VoiceDir() string {
	dir := strings.TrimSpace(c.Voice.Dir)
	if dir == "" {
		return filepath.Join(c.Data.Dir, "voices")
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(c.Data.Dir, dir)
}

// DefaultWakeThreshold is openWakeWord's own detection threshold. A config
// that omits wakeword.threshold keeps the deployed behaviour.
const DefaultWakeThreshold = 0.5

// DefaultCommandThreshold is the detection probability a command model uses
// unless wakeword.thresholds names it. A wake-free command acts with no
// confirmation, so it ships stricter than the wake word.
const DefaultCommandThreshold = 0.7

// Wakeword is the wake word service config and its model allowlist. The
// Assistant requests only these models, so a model that is not listed cannot
// wake it. Every detection then carries the model name into the turn, which
// the command models use for their wake-free actions.
type Wakeword struct {
	Addr string `yaml:"addr"` // empty disables voice input
	// Model is the legacy single-model form. WakeModels prefers Models, so an
	// existing config.yaml or WAKEWORD_MODEL deployment keeps working.
	Model string `yaml:"model"`
	// Models is the ordered allowlist of detectors to listen for.
	Models []string `yaml:"models"`
	// Threshold is the default detection probability for every model without
	// an entry in Thresholds. The service applies it per model.
	Threshold float64 `yaml:"threshold"`
	// Thresholds overrides Threshold by model name. Each name must be in the
	// allowlist or the command list: a threshold for a model nobody listens
	// for is a typo, not a setting.
	Thresholds map[string]float64 `yaml:"thresholds"`
	// Commands configures the wake-free command models. They are off by
	// default until models trained on the owner's samples and a measured
	// false-trigger rate exist.
	Commands WakeCommands `yaml:"commands"`
	Timeout  Duration     `yaml:"timeout"`
	Chime    struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"chime"`
}

// WakeCommands is the config layer for the wake-free command models (#81).
// They are detected by the same service as the wake word and handled without
// STT or routing: one bounded volume step and a short Cue.
type WakeCommands struct {
	// Enabled asks the service for the command models and registers their
	// handlers. A model that is not requested can never fire.
	Enabled bool `yaml:"enabled"`
	// Models is the ordered command-model list. Cross-checked against the wake
	// allowlist so one name cannot be both.
	Models []string `yaml:"models"`
	// Step is the volume change per detection, in percent.
	Step int `yaml:"step"`
	// Cooldown is the minimum time between accepted detections, so one
	// misheard word cannot walk the volume to zero.
	Cooldown Duration `yaml:"cooldown"`
	// Threshold is the default detection probability for a command model
	// without an entry in wakeword.thresholds.
	Threshold float64 `yaml:"threshold"`
}

// WakeModels is the resolved allowlist: Models when set, otherwise the legacy
// single Model. Names are trimmed, deduplicated and kept in configured order.
func (w Wakeword) WakeModels() []string {
	source := w.Models
	if len(source) == 0 && strings.TrimSpace(w.Model) != "" {
		source = []string{w.Model}
	}
	return normalizeModelNames(source)
}

// ModelNames is the configured command list, normalized. It is independent of
// Enabled so config validation and the Dashboard can name a model even while
// the feature is switched off.
func (c WakeCommands) ModelNames() []string { return normalizeModelNames(c.Models) }

// CommandModels is the active command allowlist. It is empty unless commands
// are enabled, so a configured-but-off list is never requested.
func (w Wakeword) CommandModels() []string {
	if !w.Commands.Enabled {
		return nil
	}
	return w.Commands.ModelNames()
}

// DetectModels is every model the Assistant requests from the service: the
// wake allowlist plus the enabled command models. The loop still receives the
// two lists separately, so a command name can never start a turn.
func (w Wakeword) DetectModels() []string {
	wake := w.WakeModels()
	commands := w.CommandModels()
	models := make([]string, 0, len(wake)+len(commands))
	models = append(models, wake...)
	models = append(models, commands...)
	return models
}

// IsCommandModel reports whether name is configured as a command model, even
// while commands are disabled. Threshold resolution and the Dashboard use it;
// only CommandModels decides what is actually requested.
func (w Wakeword) IsCommandModel(name string) bool {
	for _, model := range w.Commands.ModelNames() {
		if model == name {
			return true
		}
	}
	return false
}

func normalizeModelNames(source []string) []string {
	models := make([]string, 0, len(source))
	seen := make(map[string]bool, len(source))
	for _, name := range source {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, name)
	}
	return models
}

// ThresholdFor is the effective detection threshold for one model. A command
// model uses the stricter command default rather than the wake-word default.
func (w Wakeword) ThresholdFor(name string) float64 {
	if threshold, ok := w.Thresholds[name]; ok {
		return threshold
	}
	if w.IsCommandModel(name) {
		if w.Commands.Threshold > 0 {
			return w.Commands.Threshold
		}
		return DefaultCommandThreshold
	}
	if w.Threshold > 0 {
		return w.Threshold
	}
	return DefaultWakeThreshold
}

// ModelThresholds is the complete map sent with every detect stream. It has
// one entry per requested model, so no model can fall back, by accident, to
// another model's threshold.
func (w Wakeword) ModelThresholds() map[string]float64 {
	models := w.DetectModels()
	if len(models) == 0 {
		return nil
	}
	thresholds := make(map[string]float64, len(models))
	for _, name := range models {
		thresholds[name] = w.ThresholdFor(name)
	}
	return thresholds
}

// VoiceSamples is the Dashboard's wake-phrase sample store and the bundle
// export directory. Takes are canonical WAV files under a per-phrase
// real/positive, real/negative layout; a submitted .tar.gz is written to the
// export directory, which Compose bind-mounts so the scp copy command names a
// host path.
type VoiceSamples struct {
	// Dir holds the draft takes. Empty means data.dir/wake-samples. A relative
	// path resolves under data.dir; an absolute path is used as-is.
	Dir string `yaml:"dir"`
	// ExportDir holds finished bundles. Empty means data.dir/exports. Compose
	// sets the absolute /exports, which is a bind mount of ./exports.
	ExportDir string `yaml:"export_dir"`
	// HostDir is ExportDir as the Pi's host sees it. Empty hides the scp and
	// Taildrop copy commands, because a container path would be wrong on the
	// desktop. Set it to the host path of the bind mount.
	HostDir string `yaml:"host_dir"`
	// SSHUser and SSHHost build the scp command. Both empty hides it.
	SSHUser string `yaml:"ssh_user"`
	SSHHost string `yaml:"ssh_host"`
	// Desktop is the Taildrop target. Empty hides the taildrop command.
	Desktop string `yaml:"desktop"`
	// MaxTakes caps each kind per phrase; 0 uses the package default.
	MaxTakes int `yaml:"max_takes"`
}

// VoiceSamplesDir resolves the take store path.
func (c Config) VoiceSamplesDir() string {
	return resolveDataPath(c.Data.Dir, c.VoiceSamples.Dir, "wake-samples")
}

// VoiceSamplesExportDir resolves the bundle directory.
func (c Config) VoiceSamplesExportDir() string {
	return resolveDataPath(c.Data.Dir, c.VoiceSamples.ExportDir, "exports")
}

func resolveDataPath(dataDir, dir, fallback string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return filepath.Join(dataDir, fallback)
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(dataDir, dir)
}

// Sounds names the sound assigned to each fixed Cue role. Duration caps are
// checked when the value is assigned, never by trimming the file.
type Sounds struct {
	Wake      string `yaml:"wake"`
	TimerRing string `yaml:"timer_ring"`
	AlarmRing string `yaml:"alarm_ring"`
	Reminder  string `yaml:"reminder"`
	Error     string `yaml:"error"`
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
	cfg.Spotify = spotify.DefaultConfig()
	cfg.Tools.DefaultTimeout = Duration(2 * time.Second)
	cfg.Embed.URL = "http://embed:8085"
	cfg.Embed.Model = "sentence-transformers/all-MiniLM-L6-v2"
	cfg.Embed.Timeout = Duration(150 * time.Millisecond)
	// Shipped policy from the #26 evaluation: 0.71/0.23. These code defaults
	// are what a config that omits the semantic block gets, so they must match
	// config.yaml; a weaker margin here would silently lower the floor.
	cfg.Semantic.Threshold = 0.71
	cfg.Semantic.Margin = 0.23
	cfg.Semantic.Joiners = []string{"and", "then", "also"}
	cfg.NameMatch.Threshold = 0.60
	cfg.Quiet = Quiet{Enabled: false, Start: "22:30", End: "07:00", Volume: 35}
	cfg.Notify = notify.DefaultConfig()
	cfg.TTS.Voice = "alba"
	cfg.TTS.Timeout = Duration(30 * time.Second)
	// Custom voice states live beside the database on the data volume. Compose
	// mounts the same volume read-only into tts at /voices.
	cfg.Voice.Dir = "voices"
	cfg.Voice.TTSDir = "/voices"
	cfg.Voice.Rate = 1.0
	cfg.VoiceSamples.Dir = "wake-samples"
	cfg.VoiceSamples.ExportDir = "exports"
	cfg.VoiceSamples.MaxTakes = 100
	cfg.Playback.Binary = "pacat"
	cfg.Capture.Binary = "parec"
	cfg.Capture.Rate = 16000
	cfg.Capture.Width = 2
	cfg.Capture.Channels = 1
	cfg.Capture.FrameMS = 32
	cfg.Wakeword.Model = "clanker"
	cfg.Wakeword.Threshold = DefaultWakeThreshold
	cfg.Wakeword.Commands = WakeCommands{
		Enabled:   false,
		Models:    []string{"volume_up", "volume_down"},
		Step:      10,
		Cooldown:  Duration(1500 * time.Millisecond),
		Threshold: DefaultCommandThreshold,
	}
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

// splitList reads a comma-separated environment value, ignoring empty entries
// so an unset or trailing comma cannot create a blank host.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
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
	if v := os.Getenv("ASSISTANT_DASHBOARD_HOSTS"); v != "" {
		cfg.Dashboard.Hosts = splitList(v)
	}
	if v := os.Getenv("ASSISTANT_DASHBOARD_UI_URL"); v != "" {
		cfg.Dashboard.UIURL = v
	}
	if v := os.Getenv("ASSISTANT_DASHBOARD_UI_VERSION"); v != "" {
		cfg.Dashboard.UIVersion = v
	}
	if v := os.Getenv("ASSISTANT_DATA_DIR"); v != "" {
		cfg.Data.Dir = v
	}
	// Spotify Web API app pair. The client ID is public; there is no client
	// secret because the flow uses PKCE. The refresh token is a secret and is
	// never read from this section.
	if v := os.Getenv("SPOTIFY_CLIENT_ID"); v != "" {
		cfg.Spotify.ClientID = v
	}
	if v := os.Getenv("SPOTIFY_REDIRECT_URI"); v != "" {
		cfg.Spotify.RedirectURI = v
	}
	if v := os.Getenv("ASSISTANT_SPOTIFY_STATUS_FILE"); v != "" {
		cfg.Spotify.PairingStatusFile = v
	}
	if v := os.Getenv("SPOTIFY_DEVICE_NAME"); v != "" {
		cfg.Spotify.DeviceName = v
	}
	if v := os.Getenv("ASSISTANT_SPOTIFY_STREAM_MATCH"); v != "" {
		cfg.Spotify.StreamMatch = v
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
	if v := os.Getenv("ASSISTANT_VOICE_DIR"); v != "" {
		cfg.Voice.Dir = v
	}
	if v := os.Getenv("ASSISTANT_VOICE_TTS_DIR"); v != "" {
		cfg.Voice.TTSDir = v
	}
	if v := os.Getenv("ASSISTANT_VOICE_RATE"); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_VOICE_RATE=%q: %w", v, err)
		}
		cfg.Voice.Rate = rate
	}
	// The Dashboard recorder's store and the host-visible bundle directory.
	// The SSH and Taildrop values only build the copy command shown after
	// Submit; no request is made with them.
	if v := os.Getenv("ASSISTANT_VOICE_SAMPLES_DIR"); v != "" {
		cfg.VoiceSamples.Dir = v
	}
	if v := os.Getenv("ASSISTANT_VOICE_SAMPLES_EXPORT_DIR"); v != "" {
		cfg.VoiceSamples.ExportDir = v
	}
	if v := os.Getenv("ASSISTANT_VOICE_SAMPLES_HOST_DIR"); v != "" {
		cfg.VoiceSamples.HostDir = v
	}
	if v := os.Getenv("ASSISTANT_EXPORT_SSH_USER"); v != "" {
		cfg.VoiceSamples.SSHUser = v
	}
	if v := os.Getenv("ASSISTANT_EXPORT_SSH_HOST"); v != "" {
		cfg.VoiceSamples.SSHHost = v
	}
	if v := os.Getenv("ASSISTANT_EXPORT_DESKTOP"); v != "" {
		cfg.VoiceSamples.Desktop = v
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
	if v := os.Getenv("ASSISTANT_WAKEWORD_MODELS"); v != "" {
		cfg.Wakeword.Models = splitList(v)
		cfg.Wakeword.Model = ""
	}
	// The old single-model variable still wins, so a deployment that sets only
	// WAKEWORD_MODEL behaves exactly as before.
	if v := os.Getenv("ASSISTANT_WAKEWORD_MODEL"); v != "" {
		cfg.Wakeword.Model = v
		cfg.Wakeword.Models = nil
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_THRESHOLD"); v != "" {
		threshold, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_WAKEWORD_THRESHOLD=%q: %w", v, err)
		}
		cfg.Wakeword.Threshold = threshold
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_THRESHOLDS"); v != "" {
		thresholds, err := parseWakeThresholds(v)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_WAKEWORD_THRESHOLDS=%q: %w", v, err)
		}
		cfg.Wakeword.Thresholds = thresholds
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_COMMANDS_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_WAKEWORD_COMMANDS_ENABLED=%q: %w", v, err)
		}
		cfg.Wakeword.Commands.Enabled = enabled
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_COMMANDS"); v != "" {
		cfg.Wakeword.Commands.Models = splitList(v)
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_COMMAND_STEP"); v != "" {
		step, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_WAKEWORD_COMMAND_STEP=%q: %w", v, err)
		}
		cfg.Wakeword.Commands.Step = step
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_COMMAND_COOLDOWN"); v != "" {
		cooldown, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_WAKEWORD_COMMAND_COOLDOWN=%q: %w", v, err)
		}
		cfg.Wakeword.Commands.Cooldown = Duration(cooldown)
	}
	if v := os.Getenv("ASSISTANT_WAKEWORD_COMMAND_THRESHOLD"); v != "" {
		threshold, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: ASSISTANT_WAKEWORD_COMMAND_THRESHOLD=%q: %w", v, err)
		}
		cfg.Wakeword.Commands.Threshold = threshold
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
		if _, err := httpguard.NewHosts(c.Dashboard.Hosts); err != nil {
			return fmt.Errorf("config: dashboard.hosts: %w", err)
		}
		if strings.ContainsAny(c.Dashboard.Owner, " \t\r\n,") {
			return fmt.Errorf("config: dashboard.owner must be one Tailscale login without spaces")
		}
	}
	if strings.TrimSpace(c.Data.Dir) == "" {
		return fmt.Errorf("config: data.dir is required")
	}
	if c.VoiceSamples.MaxTakes < 0 {
		return fmt.Errorf("config: voice_samples.max_takes must be positive or zero for the default")
	}
	if c.VoiceSamples.MaxTakes > 0 && c.VoiceSamples.MaxTakes < 5 {
		return fmt.Errorf("config: voice_samples.max_takes must be at least 5 (the smallest finished set)")
	}
	if err := c.Spotify.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
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
	if c.NameMatch.Threshold <= 0 || c.NameMatch.Threshold > 1 {
		return fmt.Errorf("config: name_match.threshold must be in (0, 1]")
	}
	if err := quiet.ValidateWindow(c.Quiet.Start, c.Quiet.End); err != nil {
		return fmt.Errorf("config: quiet: %w", err)
	}
	if c.Quiet.Volume < 0 || c.Quiet.Volume > 100 {
		return fmt.Errorf("config: quiet.volume must be between 0 and 100")
	}
	if err := c.Notify.Validate(); err != nil {
		return fmt.Errorf("config: notify: %w", err)
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
	if c.Voice.Rate < 0.5 || c.Voice.Rate > 2.0 {
		return fmt.Errorf("config: voice.rate must be between 0.5 and 2.0")
	}
	if c.TTS.Addr != "" && !strings.HasPrefix(c.Voice.TTSDir, "/") {
		return fmt.Errorf("config: voice.tts_dir must be an absolute path inside the tts container")
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
	if err := validateControlPhrases(c.ControlPhrases); err != nil {
		return err
	}
	return nil
}

// validateControlPhrases checks the shared phrase table's shape. A bare value
// names one of the fixed Focus intents; a dotted value names a Tool, which the
// pipeline resolves against the registry once it is built.
func validateControlPhrases(phrases map[string]string) error {
	for phrase, raw := range phrases {
		if strings.TrimSpace(phrase) == "" {
			return fmt.Errorf("config: control_phrases has an empty phrase")
		}
		target := strings.TrimSpace(raw)
		if target == "" {
			return fmt.Errorf("config: control_phrases.%q has no target", phrase)
		}
		if !focus.IsIntentTarget(target) {
			continue
		}
		if _, ok := focus.ParseIntent(target); !ok {
			return fmt.Errorf("config: control_phrases.%q points at unknown intent %q", phrase, target)
		}
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
	if err := c.validateWakeword(); err != nil {
		return err
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

// validateWakeword holds the allowlist and thresholds to the contract the
// detect stream relies on: at least one model, and a threshold only for a
// listed model. A silent typo in either would otherwise wake the wrong model
// or leave a model at the wrong threshold.
func (c Config) validateWakeword() error {
	models := c.Wakeword.WakeModels()
	if len(models) == 0 {
		return fmt.Errorf("config: wakeword.models must name at least one model when wakeword.addr is set")
	}
	for _, name := range models {
		if strings.ContainsAny(name, " \t") {
			return fmt.Errorf("config: wakeword model %q must not contain whitespace", name)
		}
	}
	if c.Wakeword.Threshold <= 0 || c.Wakeword.Threshold > 1 {
		return fmt.Errorf("config: wakeword.threshold must be in (0, 1], got %v", c.Wakeword.Threshold)
	}
	if err := c.validateWakeCommands(models); err != nil {
		return err
	}

	// A threshold is accepted for any configured model, including a command
	// model while commands are switched off, so an operator can prepare the
	// .env before enabling the feature. Anything else is a typo.
	known := make(map[string]bool, len(models)+len(c.Wakeword.Commands.ModelNames()))
	for _, name := range models {
		known[name] = true
	}
	for _, name := range c.Wakeword.Commands.ModelNames() {
		known[name] = true
	}
	for name, threshold := range c.Wakeword.Thresholds {
		if !known[name] {
			return fmt.Errorf("config: wakeword.thresholds.%q names a model that is not in wakeword.models or wakeword.commands.models", name)
		}
		if threshold <= 0 || threshold > 1 {
			return fmt.Errorf("config: wakeword.thresholds.%q must be in (0, 1], got %v", name, threshold)
		}
	}
	return nil
}

// validateWakeCommands keeps the wake-free command list disjoint from the wake
// list and bounds the step, cooldown and threshold. A configured model is
// checked even while commands are disabled: enabling the feature later must
// not turn a typo into a live model.
func (c Config) validateWakeCommands(wakeModels []string) error {
	commands := c.Wakeword.Commands.ModelNames()
	wake := make(map[string]bool, len(wakeModels))
	for _, name := range wakeModels {
		wake[name] = true
	}
	for _, name := range commands {
		if strings.ContainsAny(name, " \t") {
			return fmt.Errorf("config: wakeword command model %q must not contain whitespace", name)
		}
		if wake[name] {
			return fmt.Errorf("config: wakeword model %q cannot be both a wake word and a command", name)
		}
	}
	if c.Wakeword.Commands.Enabled && len(commands) == 0 {
		return fmt.Errorf("config: wakeword.commands.models must name at least one model when commands are enabled")
	}
	if c.Wakeword.Commands.Step < 1 || c.Wakeword.Commands.Step > 50 {
		return fmt.Errorf("config: wakeword.commands.step must be between 1 and 50, got %d", c.Wakeword.Commands.Step)
	}
	if c.Wakeword.Commands.Cooldown.Duration() <= 0 {
		return fmt.Errorf("config: wakeword.commands.cooldown must be positive")
	}
	if c.Wakeword.Commands.Threshold <= 0 || c.Wakeword.Commands.Threshold > 1 {
		return fmt.Errorf("config: wakeword.commands.threshold must be in (0, 1], got %v", c.Wakeword.Commands.Threshold)
	}
	return nil
}

// parseWakeThresholds reads the deployment form name:value,name:value. It is
// strict on purpose: a malformed entry is a startup error, not a model that
// silently uses the wrong threshold.
func parseWakeThresholds(value string) (map[string]float64, error) {
	thresholds := make(map[string]float64)
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, raw, ok := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		raw = strings.TrimSpace(raw)
		if !ok || name == "" || raw == "" {
			return nil, fmt.Errorf("want name:value entries, got %q", entry)
		}
		if _, exists := thresholds[name]; exists {
			return nil, fmt.Errorf("duplicate threshold for %q", name)
		}
		threshold, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		thresholds[name] = threshold
	}
	if len(thresholds) == 0 {
		return nil, fmt.Errorf("no name:value entries")
	}
	return thresholds, nil
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
