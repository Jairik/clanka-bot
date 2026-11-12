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
	"path/filepath"
	"syscall"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/announce"
	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/config"
	"github.com/Jairik/clanka-bot/assistant/internal/connections"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/secretsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/settingsapi"
	"github.com/Jairik/clanka-bot/assistant/internal/sound"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/tz"
	"github.com/Jairik/clanka-bot/assistant/internal/voice"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
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
	// Keep the loaded config untouched for settings layers. cfg itself is
	// updated with effective connection values below, because those are read
	// once at startup.
	baseCfg := cfg

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// One signal context covers the HTTP listeners, the voice loop and the
	// location watch.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The one database (ADR-0003) opens before any Tool module registers so
	// every writer shares the same handle. Compose mounts its volume here;
	// migrations run now and keep the file across restarts and rebuilds.
	storePath := filepath.Join(cfg.Data.Dir, "assistant.db")
	st, err := store.Open(ctx, storePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("store close", "error", err.Error())
		}
	}()

	// Connection-level settings (devices, Wyoming endpoints, ports) are read
	// here, once. A stored override is persisted and reported as
	// restart-required; applying it would need a new device or listener, so
	// this startup resolution is the only place it takes effect.
	connSettings, err := settings.New(ctx, st, connections.Definitions(config.Default(), cfg)...)
	if err != nil {
		return fmt.Errorf("connection settings: %w", err)
	}
	connections.Apply(&cfg, connSettings)
	// Re-check the combination after overrides, so a restart cannot settle on
	// an invalid device/endpoint set.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("connection settings: %w", err)
	}

	// The Cue library indexes built-in sounds and WAV files dropped into the
	// data volume's sounds/ folder. Cues render to the capture format so the
	// wake path keeps the endpointing timing it already had (#18).
	cueFormat := audio.Format{Rate: cfg.Capture.Rate, Width: 2, Channels: 1}
	if cueFormat.Rate <= 0 {
		cueFormat.Rate = 16000
	}
	cues, err := sound.New(ctx, st, cfg.Data.Dir, cueFormat, logger)
	if err != nil {
		return fmt.Errorf("sound library: %w", err)
	}

	// The provider entry names the secret key and is needed before Tool
	// registration so the secrets service knows every writable name.
	provider, err := cfg.Agent.ActiveProvider()
	if err != nil {
		return err
	}
	toolSet := tools.NewSet()
	// The declared names are the write-only allowlist: Tool modules first, then
	// the Agent provider's key. Environment variables with the same names are
	// the bootstrap; a stored value wins.
	secretNames := append(toolSet.SecretNames(), provider.APIKeyEnv)
	secretsSvc, err := secrets.New(ctx, st.DB(), secretNames)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}

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
	// One serial sink keeps reply audio, Fillers and timer alerts from
	// overlapping on the single speaker. The wake chime uses the raw sink:
	// it plays before any reply is chosen.
	playSink := speech.NewSerialSink(sink)

	// Speech is optional: without a TTS address the debug entry stays text-only.
	// The Filler audio is synthesized after the settings service resolves the
	// live wording, so a stored override applies at startup.
	var speechSpeaker *speech.Speaker
	var ttsClient *wyoming.Client
	var ttsVoice wyoming.Voice
	if cfg.TTS.Addr != "" {
		ttsClient = wyoming.NewClient(cfg.TTS.Addr, wyoming.WithTimeout(cfg.TTS.Timeout.Duration()))
		checkTTSService(logger, ttsClient, cfg.TTS.Addr)
		ttsVoice = wyoming.Voice{Name: cfg.TTS.Voice}
		speechSpeaker = speech.New(ttsClient, playSink, ttsVoice, logger)
	}

	// One Announcement queue owns unprompted speech: it waits for the current
	// reply and any pending Confirmation, then plays one line at a time. It is
	// absent when TTS is not configured, so nothing can queue speech.
	var announcements *announce.Queue
	if speechSpeaker != nil {
		announcements = announce.New(speechSpeaker, logger, announce.WithBlocked(func() bool {
			_, pending := registry.Pending()
			return pending
		}))
		defer announcements.Close()
	}

	// One Focus registry routes the shared Control phrases to a ringing alert,
	// the current reply or music, in that fixed order. It is shared by the
	// pipeline (routing), the reply session (the current reply) and the Tool
	// modules that can hold Focus.
	focusRegistry := focus.New()

	// One module list owns Tool wiring; each module reads its tools.<name>
	// section and falls back to code defaults. The timezone seam exists before
	// modules register (they capture Deps), but it only becomes live once the
	// settings service below is created and bound.
	zone := tz.New(os.Getenv("TZ"), logger)
	deps := tool.Deps{
		Logger:        logger,
		Location:      locSvc.Current,
		Audio:         sink,
		Store:         st,
		Secrets:       secretsSvc,
		Announcements: announcements,
		Focus:         focusRegistry,
		Zone:          zone,
		Cues:          cues,
	}
	if speechSpeaker != nil {
		deps.Speaker = speechSpeaker
	}
	if err := toolSet.RegisterAll(registry, deps, cfg.Tools.Sections); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}

	// Tool enable/disable, Permission overrides and the user-facing wording all
	// share the three layers. Definitions are built before the policy is
	// installed, while registry.Info still returns the code declarations.
	wordingSetup, err := wording.Build(registry, wording.Config{
		ControlPhrases:     baseCfg.ControlPhrases,
		RequestPrefixes:    baseCfg.Agent.RequestPrefixes,
		Filler:             baseCfg.Agent.Filler,
		UnavailableMessage: baseCfg.Agent.UnavailableMessage,
	})
	if err != nil {
		return err
	}
	toolDefs, err := toolpolicy.Definitions(registry, toolpolicy.Config{
		Disabled:    baseCfg.Tools.Disabled,
		Permissions: baseCfg.Tools.Permissions,
	})
	if err != nil {
		return err
	}
	defs := append(connections.Definitions(config.Default(), baseCfg), wordingSetup.Definitions...)
	defs = append(defs, toolDefs...)
	defs = append(defs, tz.Definition(baseCfg.Timezone))
	// Sound role assignments are live settings too, so the Dashboard write and
	// the running Cue resolution share the same service.
	defs = append(defs, cues.Definitions(sound.Config{
		Wake:      baseCfg.Sounds.Wake,
		TimerRing: baseCfg.Sounds.TimerRing,
		AlarmRing: baseCfg.Sounds.AlarmRing,
		Reminder:  baseCfg.Sounds.Reminder,
		Error:     baseCfg.Sounds.Error,
	})...)
	// The model name is a live setting like the wording. Its default comes
	// from the active provider's code default when that provider is built in;
	// otherwise the configured model is both the default and the config layer.
	modelDefault := provider.Model
	if defaultProvider, err := config.Default().Agent.ActiveProvider(); err == nil && config.Default().Agent.Provider == baseCfg.Agent.Provider {
		modelDefault = defaultProvider.Model
	}
	defs = append(defs, agent.ModelDefinition(modelDefault, provider.Model))
	settingsSvc, err := settings.New(ctx, st, defs...)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	// Bind after the service exists so the timezone is live overridable: the
	// Dashboard's settings handler and any voice Tool write the same service.
	zone.Bind(settingsSvc)
	// Bind the same live settings service the Dashboard writes, so a role
	// change applies on the next Cue. The shared serial sink keeps a Cue from
	// overlapping a reply.
	cues.Bind(settingsSvc, playSink)
	if zone.IsUTC() {
		// UTC is a safe default, not a guess: warn once so a silent one-hour
		// error in a clock-time Alarm is visible at startup.
		logger.Warn("timezone is UTC: set the timezone setting or TZ so clock-time alerts use local time")
	}
	toolPolicy, err := toolpolicy.New(settingsSvc, registry)
	if err != nil {
		return fmt.Errorf("tool settings: %w", err)
	}
	// The settings API has no authentication of its own. It is mounted on the
	// localhost debug listener and, from #47, on the authenticated Dashboard.
	settingsHandler := settingsapi.New(settingsSvc, toolPolicy, logger)

	// The Filler is cached from the effective wording, not the raw config: a
	// stored override applies on the first request after a restart. Later text
	// changes re-synthesize in the background through the wording manager.
	var filler *speech.Filler
	var synthFiller wording.FillerSynth
	if ttsClient != nil {
		filler = loadAgentFiller(logger, ttsClient, ttsVoice, settingsSvc.String(wording.FillerKey), cfg.TTS.Timeout.Duration())
		synthFiller = func(ctx context.Context, text string) (*speech.Filler, error) {
			ctx, cancel := context.WithTimeout(ctx, cfg.TTS.Timeout.Duration())
			defer cancel()
			return speech.SynthesizeFiller(ctx, ttsClient, ttsVoice, text)
		}
	}
	wordingMgr, err := wording.NewManager(settingsSvc, wordingSetup, filler, synthFiller, logger)
	if err != nil {
		return err
	}
	defer wordingMgr.Close()

	// One composable mux also serves the write-only secrets routes, so both
	// surfaces get the same handlers and neither exposes them publicly.
	secretsHandler := secretsapi.New(secretsSvc, logger)
	apiHandler := http.NewServeMux()
	mountAPI := func(prefix string, handler http.Handler) {
		apiHandler.Handle(prefix, handler)
		apiHandler.Handle(prefix+"/", handler)
	}
	mountAPI("/settings", settingsHandler)
	mountAPI("/tools", settingsHandler)
	mountAPI("/secrets", secretsHandler)

	agentPath, err := agent.New(agent.Settings{
		BaseURL:   provider.BaseURL,
		APIKeyEnv: provider.APIKeyEnv,
		// Read the resolved key per request: a value stored from the Dashboard
		// takes effect on the next turn without a restart.
		APIKey: func() string {
			value, _ := secretsSvc.Get(provider.APIKeyEnv)
			return value
		},
		Model: provider.Model,
		// Read the live setting per request, so a Dashboard model change
		// applies to the next turn without rebuilding the Agent path.
		ModelFunc:          func() string { return settingsSvc.String(agent.ModelKey) },
		Headers:            provider.Headers,
		Routing:            provider.Routing,
		SystemPrompt:       cfg.Agent.SystemPrompt,
		SystemPromptFunc:   func() string { return locSvc.SystemPrompt(cfg.Agent.SystemPrompt) },
		UnavailableMessage: baseCfg.Agent.UnavailableMessage,
		// The live wording is read per failure, so a Dashboard change applies
		// to the next model failure without rebuilding the Agent path.
		UnavailableMessageFunc: wordingMgr.UnavailableMessage,
		FirstTokenTimeout:      cfg.Agent.FirstTokenTimeout.Duration(),
		RequestTimeout:         cfg.Agent.RequestTimeout.Duration(),
		MaxSteps:               cfg.Agent.MaxSteps,
		HistoryTurns:           cfg.Agent.History.Turns,
		HistoryQuiet:           cfg.Agent.History.Quiet.Duration(),
	}, registry, logger)
	if err != nil {
		return err
	}

	opts := []pipeline.Option{
		pipeline.WithAgent(agentPath, baseCfg.Agent.RequestPrefixes),
		pipeline.WithHistory(agentPath.History()),
		// Spoken Confirmation for confirm Tools, enforced by the registry.
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(registry, logger)),
		// Control phrases and Agent request prefixes are read per request.
		pipeline.WithWording(wordingMgr),
		// Shared Control phrases reach whatever holds Focus; with nothing in
		// Focus they fall through to the semantic Fast path.
		pipeline.WithFocus(focusRegistry),
	}
	// Compute Tool example vectors from the effective Examples (a stored
	// override may already exist). A dead embed service only disables semantic
	// matching, leaving Control phrases and the Agent path working.
	semantic := pipeline.NewSemanticRouter(registry, embed.NewClient(cfg.Embed.URL, cfg.Embed.Model), pipeline.SemanticConfig{
		Threshold: cfg.Semantic.Threshold,
		Margin:    cfg.Semantic.Margin,
		Joiners:   cfg.Semantic.Joiners,
		Timeout:   cfg.Embed.Timeout.Duration(),
	})
	semantic.SetExamplesSource(wordingMgr)
	initialExamples := wordingMgr.Examples()
	buildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	exampleSet, err := semantic.BuildSet(buildCtx, initialExamples)
	cancel()
	var liveRouter *pipeline.SemanticRouter
	if err != nil {
		logger.Warn("semantic routing disabled", "embed_url", cfg.Embed.URL, "error", err.Error())
	} else {
		semantic.Swap(exampleSet)
		wordingMgr.MarkExamplesBuilt(initialExamples)
		opts = append(opts, pipeline.WithSemanticRouter(semantic))
		liveRouter = semantic
	}
	// The worker only watches wording changes; the startup build above is the
	// initial state. A nil router still lets the Filler reload.
	wordingMgr.Start(ctx, liveRouter)

	p, err := pipeline.New(registry, baseCfg.ControlPhrases, logger, opts...)
	if err != nil {
		return err
	}

	// One Session orchestrates every spoken turn: the voice loop and the typed
	// debug entry both call Ask, so filler, streaming and playback never split.
	var speaker reply.Speaker
	if speechSpeaker != nil {
		speaker = speechSpeaker
	}
	var sessionOpts []reply.Option
	if announcements != nil {
		// Every turn holds the Announcement queue, so unprompted speech only
		// plays between turns and once a pending Confirmation resolves.
		sessionOpts = append(sessionOpts, reply.WithAnnouncements(announcements))
	}
	// The error Cue plays before an Agent-path Unavailable message. Other
	// consumers come from the same library via tool.Deps.Cues.
	sessionOpts = append(sessionOpts, reply.WithCues(cues))
	// The Filler is read once per turn from the live source, so a re-synthesized
	// phrase is heard from the next request onward.
	sessionOpts = append(sessionOpts, reply.WithFillerSource(wordingMgr))
	// A speaking reply holds Focus, so a shared "stop" cancels its model stream
	// and playback rather than the stop request itself.
	sessionOpts = append(sessionOpts, reply.WithFocus(focusRegistry))
	session := reply.New(p, speaker, playSink, filler, sessionOpts...)

	// The debug entry is only spoken when a TTS service is configured.
	var responder api.Responder
	if speechSpeaker != nil {
		responder = session
	}

	if cfg.VoiceEnabled() {
		// Read the wake role per detection so a Dashboard write applies live.
		// wakeword.chime.enabled stays the master switch it was before #55.
		wakeCue := func() ([]byte, audio.Format, bool) {
			if !cfg.Wakeword.Chime.Enabled {
				return nil, audio.Format{}, false
			}
			clip, ok := cues.Clip(sound.RoleWake)
			if !ok {
				return nil, audio.Format{}, false
			}
			return clip.PCM, clip.Format, true
		}
		loop, err := newVoiceLoop(cfg, session, p, sink, announcements, wakeCue, logger)
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
		if announcements != nil {
			// The debug entry can queue an Announcement, so the between-turns
			// behaviour is demonstrable without a Reminder.
			opts = append(opts, api.WithAnnouncements(announcements))
		}
		// Settings, Tool-override and secret writes live on the localhost
		// debug listener and on the authenticated Dashboard. The health
		// listener never mounts this handler.
		opts = append(opts, api.WithAPI(apiHandler))
		servers = append(servers, &http.Server{
			Addr:              cfg.Debug.Addr,
			Handler:           api.NewHandler(p, logger, true, opts...),
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	// The Dashboard is a separate listener and is absent until a Tailscale
	// owner is configured. Its source and identity checks live in the handler,
	// so this listener is never an unauthenticated surface.
	if cfg.Dashboard.Enabled() {
		dashboardHandler, err := dashboard.New(dashboard.Options{
			Owner:         cfg.Dashboard.Owner,
			Logger:        logger,
			HealthTimeout: cfg.Dashboard.HealthTimeout.Duration(),
			Services:      dashboard.ComposeServiceChecks(cfg),
			Modules:       toolSet,
			Location:      locSvc,
			Settings:      settingsSvc,
			ToolPolicy:    toolPolicy,
			API:           apiHandler,
		})
		if err != nil {
			return err
		}
		servers = append(servers, &http.Server{
			Addr:              cfg.Dashboard.Addr,
			Handler:           dashboardHandler,
			ReadHeaderTimeout: 5 * time.Second,
		})
		logger.Info("dashboard enabled",
			"addr", cfg.Dashboard.Addr,
			"owner", cfg.Dashboard.Owner,
		)
	} else {
		logger.Info("dashboard disabled", "reason", "dashboard.owner is empty")
	}

	logger.Info("assistant started",
		"http_addr", cfg.HTTP.Addr,
		"debug_enabled", cfg.Debug.Enabled,
		"debug_addr", cfg.Debug.Addr,
		"data_dir", cfg.Data.Dir,
		"store", st.Path(),
		"settings", len(settingsSvc.List()),
		"sounds", len(cues.List()),
		"timezone", zone.Name(),
		"speech", cfg.TTS.Addr != "",
		"tts_addr", cfg.TTS.Addr,
		"pulse_sink", cfg.Playback.Sink,
		"pulse_source", cfg.Capture.Source,
		"voice", cfg.VoiceEnabled(),
		"wakeword_addr", cfg.Wakeword.Addr,
		"stt_addr", cfg.STT.Addr,
		"agent_filler", settingsSvc.String(wording.FillerKey),
		"tools", len(registry.Names()),
		"focus_intents", toolSet.FocusIntents(),
		"embed_model", cfg.Embed.Model,
		"semantic_tools", semantic.ToolCount(),
		"location_city", currentCity(locSvc),
	)

	return serve(ctx, logger, servers)
}

// newVoiceLoop wires the microphone path: Pulse capture, Wyoming wake word and
// STT, the live wake Cue, and the shared reply Session. The pipeline is the
// Barge-in seam that drops a waiting Confirmation, and the Announcement queue
// stops unprompted speech as soon as a wake word starts a turn.
func newVoiceLoop(cfg config.Config, session voice.Asker, pending voice.PendingDropper, sink audio.Sink, announcements *announce.Queue, wakeCue func() ([]byte, audio.Format, bool), logger *slog.Logger) (*voice.Loop, error) {
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

	voiceCfg := voice.Config{
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
	logger.Info("voice input enabled",
		"pulse_source", cfg.Capture.Source,
		"wakeword_addr", cfg.Wakeword.Addr,
		"wakeword_model", cfg.Wakeword.Model,
		"stt_addr", cfg.STT.Addr,
		"stt_model", cfg.STT.Model,
		"endpoint_silence", cfg.STT.Endpoint.Silence.Duration().String(),
	)

	deps := voice.Deps{
		Source:    audio.NewPulseSource(cfg.Capture.Binary, cfg.Capture.Source, captureFormat),
		Wake:      voice.NewWyomingWake(wakeClient, names, wireFormat),
		STT:       voice.NewWyomingSTT(sttClient, wyoming.TranscribeOptions{Name: cfg.STT.Model, Language: cfg.STT.Language}, wireFormat),
		Player:    audio.NewPCMPlayer(sink),
		Responder: voice.NewPipelineResponder(session, tool.DevicePi),
		Dropper:   pending,
		WakeCue:   wakeCue,
		Logger:    logger,
	}
	// Leave the seam absent rather than storing a typed nil: a text-only setup
	// has no speech to stop.
	if announcements != nil {
		deps.Announcer = announcements
	}
	return voice.New(voiceCfg, deps)
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
