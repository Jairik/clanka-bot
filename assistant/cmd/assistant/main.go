// Command assistant runs the middleware: health endpoint and localhost debug
// entry for typed transcripts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "assistant:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultConfig := os.Getenv("ASSISTANT_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "config.yaml"
	}
	configPath := flag.String("config", defaultConfig, "path to the YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// One signal context covers the HTTP listeners, the voice loop and the
	// location watch.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resolve the place once before serving so the first request has it; Watch
	// keeps it fresh every 24h. The prompt reads the service per request.
	locSvc := location.NewService(
		location.NewIPWhoIs(&http.Client{Timeout: cfg.Location.Timeout.Duration()}, cfg.Location.ProviderURL),
		location.Location{
			City:     cfg.Location.Fallback.City,
			Region:   cfg.Location.Fallback.Region,
			Timezone: cfg.Location.Fallback.Timezone,
		},
		cfg.Location.RefreshInterval.Duration(),
		logger,
	)
	lookupCtx, cancelLookup := context.WithTimeout(ctx, cfg.Location.Timeout.Duration())
	locSvc.Refresh(lookupCtx)
	cancelLookup()
	go locSvc.Watch(ctx)
	registry := tool.NewRegistry(cfg.Tools.DefaultTimeout.Duration())

	// One sink serves reply playback, the wake chime, timer alerts and the
	// audio Tools, so audio.stop can cancel whatever the assistant is playing.
	sink := audio.NewPulseSink(cfg.Playback.Binary, cfg.Playback.Sink)

	// Speech is optional: without a TTS address the debug entry stays text-only.
	// The Filler is synthesized once here, from the config read at startup, so a
	// phrase change is picked up by a restart rather than live.
	var speechSpeaker *speech.Speaker
	var filler *speech.Filler
	if cfg.TTS.Addr != "" {
		client := wyoming.NewClient(cfg.TTS.Addr, wyoming.WithTimeout(cfg.TTS.Timeout.Duration()))
		checkTTSService(logger, client, cfg.TTS.Addr)
		voice := wyoming.Voice{Name: cfg.TTS.Voice}
		speechSpeaker = speech.New(client, sink, voice, logger)
		filler = loadAgentFiller(logger, client, voice, cfg.Agent.Filler, cfg.TTS.Timeout.Duration())
	}

	// Timers reuse the reply Speaker for their alert.
	var timerAlert func(context.Context, string) error
	if speechSpeaker != nil {
		timerAlert = speechSpeaker.SayText
	}
	if err := tools.RegisterAll(registry, tools.Options{
		Weather:    weather.Deps{Location: locSvc.Current},
		Audio:      sink,
		TimerAlert: timerAlert,
		Logger:     logger,
	}); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}

	provider, err := cfg.Agent.ActiveProvider()
	if err != nil {
		return err
	}
	agentPath, err := agent.New(agent.Settings{
		BaseURL:            provider.BaseURL,
		APIKeyEnv:          provider.APIKeyEnv,
		Model:              provider.Model,
		Headers:            provider.Headers,
		Routing:            provider.Routing,
		SystemPrompt:       cfg.Agent.SystemPrompt,
		SystemPromptFunc:   func() string { return locSvc.SystemPrompt(cfg.Agent.SystemPrompt) },
		UnavailableMessage: cfg.Agent.UnavailableMessage,
		FirstTokenTimeout:  cfg.Agent.FirstTokenTimeout.Duration(),
		RequestTimeout:     cfg.Agent.RequestTimeout.Duration(),
		MaxSteps:           cfg.Agent.MaxSteps,
		HistoryTurns:       cfg.Agent.History.Turns,
		HistoryQuiet:       cfg.Agent.History.Quiet.Duration(),
	}, registry, logger)
	if err != nil {
		return err
	}

	opts := []pipeline.Option{
		pipeline.WithAgent(agentPath, cfg.Agent.RequestPrefixes),
		pipeline.WithHistory(agentPath.History()),
		// Spoken Confirmation for confirm Tools, enforced by the registry.
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(registry, logger)),
	}
	// Compute Tool example vectors once; a dead embed service only disables
	// semantic matching, leaving Control phrases and the Agent path working.
	semantic := pipeline.NewSemanticRouter(registry, embed.NewClient(cfg.Embed.URL, cfg.Embed.Model), pipeline.SemanticConfig{
		Threshold: cfg.Semantic.Threshold,
		Margin:    cfg.Semantic.Margin,
		Joiners:   cfg.Semantic.Joiners,
		Timeout:   cfg.Embed.Timeout.Duration(),
	})
	buildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = semantic.Build(buildCtx)
	cancel()
	if err != nil {
		logger.Warn("semantic routing disabled", "embed_url", cfg.Embed.URL, "error", err.Error())
	} else {
		opts = append(opts, pipeline.WithSemanticRouter(semantic))
	}

	p, err := pipeline.New(registry, cfg.ControlPhrases, logger, opts...)
	if err != nil {
		return err
	}

	// One Session orchestrates every spoken turn: the voice loop and the typed
	// debug entry both call Ask, so filler, streaming and playback never split.
	var speaker reply.Speaker
	if speechSpeaker != nil {
		speaker = speechSpeaker
	}
	session := reply.New(p, speaker, sink, filler)

	// The debug entry is only spoken when a TTS service is configured.
	var responder api.Responder
	if speechSpeaker != nil {
		responder = session
	}

	if cfg.VoiceEnabled() {
		loop, err := newVoiceLoop(cfg, session, sink, logger)
		if err != nil {
			return err
		}
		go func() {
			if err := loop.Run(ctx); err != nil {
				logger.Error("voice loop stopped", "error", err.Error())
			}
		}()
	}

	// The health listener never serves the debug route; the debug listener does.
	servers := []*http.Server{{
		Addr:              cfg.HTTP.Addr,
		Handler:           api.NewHandler(p, logger, false),
		ReadHeaderTimeout: 5 * time.Second,
	}}
	if cfg.Debug.Enabled {
		var opts []api.Option
		if responder != nil {
			opts = append(opts, api.WithResponder(responder))
		}
		servers = append(servers, &http.Server{
			Addr:              cfg.Debug.Addr,
			Handler:           api.NewHandler(p, logger, true, opts...),
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	logger.Info("assistant started",
		"http_addr", cfg.HTTP.Addr,
		"debug_enabled", cfg.Debug.Enabled,
		"debug_addr", cfg.Debug.Addr,
		"speech", cfg.TTS.Addr != "",
		"tts_addr", cfg.TTS.Addr,
		"pulse_sink", cfg.Playback.Sink,
		"pulse_source", cfg.Capture.Source,
		"voice", cfg.VoiceEnabled(),
		"wakeword_addr", cfg.Wakeword.Addr,
		"stt_addr", cfg.STT.Addr,
		"agent_filler", cfg.Agent.Filler,
		"tools", len(registry.Names()),
		"embed_model", cfg.Embed.Model,
		"semantic_tools", semantic.ToolCount(),
		"location_city", currentCity(locSvc),
	)

	return serve(ctx, logger, servers)
}

// newVoiceLoop wires the microphone path: Pulse capture, Wyoming wake word and
// STT, the wake chime, and the shared reply Session.
func newVoiceLoop(cfg config.Config, session voice.Asker, sink audio.Sink, logger *slog.Logger) (*voice.Loop, error) {
	captureFormat := audio.Format{Rate: cfg.Capture.Rate, Width: cfg.Capture.Width, Channels: cfg.Capture.Channels}
	frameBytes := cfg.Capture.Rate * cfg.Capture.Width * cfg.Capture.Channels * cfg.Capture.FrameMS / 1000
	if frameBytes <= 0 {
		return nil, fmt.Errorf("voice: capture frame_ms %d is too small", cfg.Capture.FrameMS)
	}
	wireFormat := wyoming.Format{Rate: captureFormat.Rate, Width: captureFormat.Width, Channels: captureFormat.Channels}

	var names []string
	if cfg.Wakeword.Model != "" {
		names = []string{cfg.Wakeword.Model}
	}
	wakeClient := wyoming.NewClient(cfg.Wakeword.Addr, wyoming.WithTimeout(cfg.Wakeword.Timeout.Duration()))
	sttClient := wyoming.NewClient(cfg.STT.Addr, wyoming.WithTimeout(cfg.STT.Timeout.Duration()))

	config := voice.Config{
		FrameBytes: frameBytes,
		Endpoint: voice.EndpointConfig{
			Rate:          captureFormat.Rate,
			Silence:       cfg.STT.Endpoint.Silence.Duration(),
			ThresholdDBFS: cfg.STT.Endpoint.ThresholdDBFS,
			MinSpeech:     cfg.STT.Endpoint.MinSpeech.Duration(),
			MaxUtterance:  cfg.STT.Endpoint.MaxUtterance.Duration(),
		},
		ChimeFormat: audio.Format{Rate: captureFormat.Rate, Width: 2, Channels: 1},
	}
	if cfg.Wakeword.Chime.Enabled {
		config.Chime = audio.Chime(captureFormat.Rate)
	}

	logger.Info("voice input enabled",
		"pulse_source", cfg.Capture.Source,
		"wakeword_addr", cfg.Wakeword.Addr,
		"wakeword_model", cfg.Wakeword.Model,
		"stt_addr", cfg.STT.Addr,
		"stt_model", cfg.STT.Model,
		"endpoint_silence", cfg.STT.Endpoint.Silence.Duration().String(),
	)

	return voice.New(config, voice.Deps{
		Source:    audio.NewPulseSource(cfg.Capture.Binary, cfg.Capture.Source, captureFormat),
		Wake:      voice.NewWyomingWake(wakeClient, names, wireFormat),
		STT:       voice.NewWyomingSTT(sttClient, wyoming.TranscribeOptions{Name: cfg.STT.Model, Language: cfg.STT.Language}, wireFormat),
		Player:    audio.NewPCMPlayer(sink),
		Responder: voice.NewPipelineResponder(session, tool.DevicePi),
		Logger:    logger,
	})
}

// loadAgentFiller caches the Agent path cue before the HTTP server starts. A
// TTS failure is not fatal: replies still work, just without the immediate cue.
func loadAgentFiller(logger *slog.Logger, client *wyoming.Client, voice wyoming.Voice, text string, timeout time.Duration) *speech.Filler {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	filler, err := speech.SynthesizeFiller(ctx, client, voice, text)
	if err != nil {
		logger.Warn("agent filler unavailable", "error", err.Error())
		return nil
	}
	logger.Info("agent filler cached", "text", filler.Text, "audio_bytes", len(filler.PCM))
	return filler
}

// checkTTSService logs whether the TTS service answers describe. A failure is
// not fatal: the text reply still works and compose gates startup anyway.
func checkTTSService(logger *slog.Logger, client *wyoming.Client, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := client.Describe(ctx)
	if err != nil {
		logger.Warn("tts not ready", "addr", addr, "error", err.Error())
		return
	}
	voices := 0
	streaming := false
	for _, program := range info.TTS {
		voices += len(program.Voices)
		streaming = streaming || program.SupportsSynthesizeStreaming
	}
	logger.Info("tts ready", "addr", addr, "programs", len(info.TTS), "voices", voices, "streaming", streaming)
}

// currentCity returns the resolved city for the startup log, or "" when no
// place is known. Coordinates are never available.
func currentCity(service *location.Service) string {
	loc, ok := service.Current()
	if !ok {
		return ""
	}
	return loc.City
}

// serve runs every HTTP server until a signal or a listener failure, then
// shuts them down together.
func serve(ctx context.Context, logger *slog.Logger, servers []*http.Server) error {
	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		go func(s *http.Server) {
			err := s.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}(srv)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown", "addr", srv.Addr, "error", err.Error())
		}
	}
	return serveErr
}
