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
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
	"github.com/Jairik/clanka-bot/assistant/tools"
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

	registry := tool.NewRegistry(cfg.Tools.DefaultTimeout.Duration())
	if err := tools.RegisterAll(registry, nil); err != nil {
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

	p, err := pipeline.New(registry, cfg.ControlPhrases, logger,
		pipeline.WithAgent(agentPath, cfg.Agent.RequestPrefixes),
		pipeline.WithHistory(agentPath.History()),
	)
	if err != nil {
		return err
	}

	// Speech is optional: without a TTS address the debug entry stays text-only.
	var sayer api.Sayer
	if cfg.TTS.Addr != "" {
		client := wyoming.NewClient(cfg.TTS.Addr, wyoming.WithTimeout(cfg.TTS.Timeout.Duration()))
		checkTTSService(logger, client, cfg.TTS.Addr)
		sayer = speech.New(
			client,
			audio.NewPulseSink(cfg.Playback.Binary, cfg.Playback.Sink),
			wyoming.Voice{Name: cfg.TTS.Voice},
			logger,
		)
	}

	// The health listener never serves the debug route; the debug listener does.
	servers := []*http.Server{{
		Addr:              cfg.HTTP.Addr,
		Handler:           api.NewHandler(p, logger, false),
		ReadHeaderTimeout: 5 * time.Second,
	}}
	if cfg.Debug.Enabled {
		servers = append(servers, &http.Server{
			Addr:              cfg.Debug.Addr,
			Handler:           api.NewHandler(p, logger, true, api.WithSpeech(sayer)),
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
		"tools", len(registry.Names()),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, logger, servers)
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
