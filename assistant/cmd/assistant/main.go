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
	if err := tools.RegisterAll(registry, tools.Options{
		Weather: weather.Deps{Location: locSvc.Current},
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

	// Speech is optional: without a TTS address the debug entry stays text-only.
	var responder api.Responder
	if cfg.TTS.Addr != "" {
		client := wyoming.NewClient(cfg.TTS.Addr, wyoming.WithTimeout(cfg.TTS.Timeout.Duration()))
		checkTTSService(logger, client, cfg.TTS.Addr)
		voice := wyoming.Voice{Name: cfg.TTS.Voice}
		sink := audio.NewPulseSink(cfg.Playback.Binary, cfg.Playback.Sink)
		// The Filler is synthesized once here, from the config read at startup,
		// so a phrase change is picked up by a restart rather than live.
		filler := loadAgentFiller(logger, client, voice, cfg.Agent.Filler, cfg.TTS.Timeout.Duration())
		responder = reply.New(p, speech.New(client, sink, voice, logger), sink, filler)
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
		"agent_filler", cfg.Agent.Filler,
		"tools", len(registry.Names()),
		"embed_model", cfg.Embed.Model,
		"semantic_tools", semantic.ToolCount(),
		"location_city", currentCity(locSvc),
	)

	return serve(ctx, logger, servers)
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
