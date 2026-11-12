// Package connections turns the connection-level config values into settings
// definitions. They are read once at startup: changing a stored override is
// stored and reported, but only a restart opens the new device, endpoint or
// port. Marking them restart_required is the honest contract; nothing here is
// hot-reloaded.
package connections

import (
	"fmt"

	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
)

// Settings keys. They mirror the config.yaml path where one exists.
const (
	HTTPAddrKey      = "http.addr"
	DebugAddrKey     = "debug.addr"
	DashboardAddrKey = "dashboard.addr"
	TTSAddrKey       = "tts.addr"
	WakewordAddrKey  = "wakeword.addr"
	STTAddrKey       = "stt.addr"
	EmbedURLKey      = "embed.url"
	CaptureSourceKey = "capture.source"
	PlaybackSinkKey  = "playback.sink"
)

// Definitions builds every connection-level setting. defaults supplies the
// code layer; cfg supplies the config.yaml (and environment) layer. main reads
// the effective values once, before it builds any device or listener.
func Definitions(defaults, cfg config.Config) []settings.Definition {
	restart := settings.WithKind(settings.RestartRequired)

	debugValidator := func(value any) error {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("want a string, got %T", value)
		}
		if cfg.Debug.Enabled && text == "" {
			return fmt.Errorf("debug.addr is required when the debug entry is enabled")
		}
		return nil
	}

	dashboardOptions := []settings.Option{
		restart,
		settings.WithConfig(cfg.Dashboard.Addr),
		settings.WithDescription("Dashboard listener. Changing the port needs a restart."),
	}
	if cfg.Dashboard.Enabled() {
		dashboardOptions = append(dashboardOptions, settings.WithValidator(func(value any) error {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("want a string, got %T", value)
			}
			if text == "" {
				return fmt.Errorf("dashboard.addr is required when the Dashboard is enabled")
			}
			// The same loopback rule as config.Load: a stored override must
			// never widen the listener past what config may bind.
			return config.ValidateDashboardAddr(text)
		}))
	}

	return []settings.Definition{
		settings.String(HTTPAddrKey, defaults.HTTP.Addr,
			restart,
			settings.WithConfig(cfg.HTTP.Addr),
			settings.WithDescription("Health listener address. Changing the port needs a restart."),
			settings.WithValidator(nonBlank),
		),
		settings.String(DebugAddrKey, defaults.Debug.Addr,
			restart,
			settings.WithConfig(cfg.Debug.Addr),
			settings.WithDescription("Localhost debug listener address. Changing the port needs a restart."),
			settings.WithValidator(debugValidator),
		),
		settings.String(DashboardAddrKey, defaults.Dashboard.Addr,
			dashboardOptions...),
		settings.String(TTSAddrKey, defaults.TTS.Addr,
			restart,
			settings.WithConfig(cfg.TTS.Addr),
			settings.WithDescription("Wyoming TTS endpoint. Changing it needs a restart."),
		),
		settings.String(WakewordAddrKey, defaults.Wakeword.Addr,
			restart,
			settings.WithConfig(cfg.Wakeword.Addr),
			settings.WithDescription("Wyoming wake word endpoint. Changing it needs a restart."),
		),
		settings.String(STTAddrKey, defaults.STT.Addr,
			restart,
			settings.WithConfig(cfg.STT.Addr),
			settings.WithDescription("Wyoming STT endpoint. Changing it needs a restart."),
		),
		settings.String(EmbedURLKey, defaults.Embed.URL,
			restart,
			settings.WithConfig(cfg.Embed.URL),
			settings.WithDescription("Embedding service URL. Changing it needs a restart."),
			settings.WithValidator(nonBlank),
		),
		settings.String(CaptureSourceKey, defaults.Capture.Source,
			restart,
			settings.WithConfig(cfg.Capture.Source),
			settings.WithDescription("Pulse microphone source. Changing it needs a restart."),
		),
		settings.String(PlaybackSinkKey, defaults.Playback.Sink,
			restart,
			settings.WithConfig(cfg.Playback.Sink),
			settings.WithDescription("Pulse playback sink. Changing it needs a restart."),
		),
	}
}

// Apply resolves every connection setting onto cfg. main calls it once before
// building devices and listeners; no component rereads these values.
func Apply(cfg *config.Config, svc *settings.Service) {
	if cfg == nil || svc == nil {
		return
	}
	cfg.HTTP.Addr = svc.String(HTTPAddrKey)
	cfg.Debug.Addr = svc.String(DebugAddrKey)
	cfg.Dashboard.Addr = svc.String(DashboardAddrKey)
	cfg.TTS.Addr = svc.String(TTSAddrKey)
	cfg.Wakeword.Addr = svc.String(WakewordAddrKey)
	cfg.STT.Addr = svc.String(STTAddrKey)
	cfg.Embed.URL = svc.String(EmbedURLKey)
	cfg.Capture.Source = svc.String(CaptureSourceKey)
	cfg.Playback.Sink = svc.String(PlaybackSinkKey)
}

// nonBlank is the shared validator for addresses and URLs that must not be
// emptied while in use.
func nonBlank(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("want a string, got %T", value)
	}
	if text == "" {
		return fmt.Errorf("must not be empty")
	}
	return nil
}
