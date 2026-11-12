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
	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/namematch"
	"github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/playback"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/ring"
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
	"github.com/Jairik/clanka-bot/assistant/internal/voicelib"
	"github.com/Jairik/clanka-bot/assistant/internal/voicesample"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
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

	// Speech is optional: without a TTS address the debug entry stays
	// text-only. The client is created before the voice library so startup can
	// learn the model revision and the built-in voices the image warmed.
	var ttsClient *wyoming.Client
	var ttsInfo wyoming.VoiceLibrary
	var ttsDescribe wyoming.Info
	if cfg.TTS.Addr != "" {
		ttsClient = wyoming.NewClient(cfg.TTS.Addr, wyoming.WithTimeout(cfg.TTS.Timeout.Duration()))
		ttsDescribe, ttsInfo = checkTTSService(logger, ttsClient, cfg.TTS.Addr)
	}

	// Describe is a second source when an older TTS service has no voice-info
	// event; its advertised voices are still the warm built-ins.
	if len(ttsInfo.Voices) == 0 {
		for _, program := range ttsDescribe.TTS {
			for _, advertised := range program.Voices {
				ttsInfo.Voices = append(ttsInfo.Voices, advertised.Name)
			}
		}
	}

	// The voice library owns the copied .safetensors states and their validation
	// revisions. It indexes files now; Rescan below runs the model smoke test
	// against the live TTS revision once the settings service exists.
	voiceSvc, err := voicelib.New(ctx, st, voicelib.Config{
		Dir:      cfg.VoiceDir(),
		TTSDir:   cfg.Voice.TTSDir,
		Default:  cfg.TTS.Voice,
		Builtins: ttsInfo.Voices,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("voice library: %w", err)
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
	// The config-layer Cue defaults also serve startup recovery, before the
	// live settings service exists, so a recovered Timer rings its sound.
	cueConfig := sound.Config{
		Wake:      baseCfg.Sounds.Wake,
		TimerRing: baseCfg.Sounds.TimerRing,
		AlarmRing: baseCfg.Sounds.AlarmRing,
		Reminder:  baseCfg.Sounds.Reminder,
		Error:     baseCfg.Sounds.Error,
	}

	// Startup recovery needs the live Ring behaviour and Quiet volume before
	// Tool modules register: a Timer overdue from the previous run must not
	// blast the code-default volume. This small reader sees the saved
	// overrides; the full settings service below replaces it for the running
	// process. It is read-only, so two views of the same DB cannot diverge.
	zone := tz.New(os.Getenv("TZ"), logger)
	recoveryDefs := append(ring.Definitions(), quiet.Definitions(baseCfg.Quiet)...)
	recoveryDefs = append(recoveryDefs, notify.Definitions(baseCfg.Notify)...)
	recoveryDefs = append(recoveryDefs, tz.Definition(baseCfg.Timezone))
	recoverySettings, err := settings.New(ctx, st, recoveryDefs...)
	if err != nil {
		return fmt.Errorf("recovery settings: %w", err)
	}
	zone.Bind(recoverySettings)
	// Quiet hours is created before the Ring engine so recovery Rings already
	// read the saved quiet.volume. Bind later swaps in the full service.
	quietSvc := quiet.New(quiet.Options{
		Reader:   recoverySettings,
		Now:      time.Now,
		Location: zone.Location,
		Logger:   logger,
	})

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
	// Notifications are created before Tool registration so a Timer/Alarm
	// recovered during Register already has the missed hook. The startup reader
	// supplies the saved notify.* values until the full service is bound below.
	notifySvc, err := notify.New(notify.Options{
		Reader:  recoverySettings,
		Quiet:   quietSvc,
		Secrets: secretsSvc,
		Logger:  logger,
		Config:  baseCfg.Notify,
	})
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	// The previous reply stays in memory with its provenance so a follow-up
	// "send that to my phone" can resolve it without persisting private text.
	lastReplyStore := lastreply.New()

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
	// live wording, so a stored override applies at startup. The speaker's
	// voice/rate source is bound to the settings service below.
	var speechSpeaker *speech.Speaker
	ttsVoice := wyoming.Voice{Name: cfg.TTS.Voice}
	if ttsClient != nil {
		speechSpeaker = speech.New(ttsClient, playSink, ttsVoice, logger)
	}

	// The speech gate lets a Ring wait for the current reply sentence and then
	// take the one speaker; the reply's next sentence waits for the Ring.
	speechGate := playback.NewGate()

	// One Focus registry routes the shared Control phrases to a ringing alert,
	// the current reply or music, in that fixed order. It is shared by the
	// pipeline (routing), the reply session (the current reply) and the Tool
	// modules that can hold Focus.
	focusRegistry := focus.New()

	// One Announcement queue owns unprompted speech and Dashboard previews: it
	// waits for the current reply, a pending Confirmation and a ringing alert,
	// then plays one item at a time. With a Dashboard but no TTS it still runs
	// for Pi previews (WithPlayback); text announcements remain unavailable.
	var announcements *announce.Queue
	if speechSpeaker != nil || cfg.Dashboard.Enabled() {
		var speaker announce.Speaker
		queueOpts := []announce.Option{
			announce.WithBlocked(func() bool {
				_, pending := registry.Pending()
				return pending || focusRegistry.Held(focus.Ring)
			}),
			// A Ring preempts a long Pi preview instead of waiting behind it.
			announce.WithPreempt(func() bool { return focusRegistry.Held(focus.Ring) }),
			// Quiet hours holds text announcements, then releases them as one
			// summary. Audio previews keep their own slots.
			announce.WithHoldSummary(),
		}
		if speechSpeaker != nil {
			// Announcements use the same speech gate as replies: a Ring waits
			// for the current line and a line waits for an active Ring.
			speaker = playback.NewGatedSpeaker(speechSpeaker, speechGate)
		} else {
			queueOpts = append(queueOpts, announce.WithPlayback())
		}
		announcements = announce.New(speaker, logger, queueOpts...)
		defer announcements.Close()
	}

	// The Ring engine exists before Tool modules register, so an overdue Timer
	// that fires during recovery already Rings instead of using the old spoken
	// alert. Its settings reader is late-bound: the settings service is created
	// below, and a startup-time Ring before that falls back to code defaults.
	var settingsSvc *settings.Service
	ringDeps := ring.Deps{
		Focus:    focusRegistry,
		Boundary: speechGate,
		Settings: func(kind ring.Kind) ring.Settings {
			var reader ring.Reader = recoverySettings
			if settingsSvc != nil {
				reader = settingsSvc
			}
			return quietSvc.RingSettings(kind, ring.ReadSettings(reader, kind))
		},
		// Timer labels follow quiet.volume; an Alarm label stays at normal
		// volume, including a Ring recovered during startup.
		SpeechGain: quietSvc.RingSpeechGain,
		Clip: func(kind ring.Kind) (sound.Clip, bool) {
			role := sound.RoleTimerRing
			if kind == ring.Alarm {
				role = sound.RoleAlarmRing
			}
			if settingsSvc == nil {
				// Startup recovery rings before the live settings service exists.
				return cues.DefaultClip(cueConfig, role)
			}
			return cues.Clip(role)
		},
		Play: func(ctx context.Context, clip sound.Clip) error {
			return clip.Play(ctx, playSink)
		},
		Logger: logger,
	}
	if speechSpeaker != nil {
		ringDeps.Speaker = speechSpeaker
	}
	ringEngine := ring.New(ringDeps)

	// One module list owns Tool wiring; each module reads its tools.<name>
	// section and falls back to code defaults. The timezone seam exists before
	// modules register (they capture Deps), but it only becomes live once the
	// settings service below is created and bound.
	//
	// One matcher serves sound, voice and playlist names. Its threshold is
	// bound to the settings service below, after the definitions are built;
	// until then it uses the config value as its fallback.
	nameThreshold := namematch.NewThreshold(baseCfg.NameMatch.Threshold)
	embedClient := embed.NewClient(cfg.Embed.URL, cfg.Embed.Model)
	names := namematch.New(embedClient, namematch.Config{Threshold: nameThreshold.Get})
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
		Ring:          ringEngine,
		Names:         names,
		Quiet:         quietSvc,
		Voices:        voiceSvc,
		Notify:        notifySvc,
		LastReply:     lastReplyStore,
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
	defs = append(defs, namematch.Definition(baseCfg.NameMatch.Threshold))
	// Sound role assignments are live settings too, so the Dashboard write and
	// the running Cue resolution share the same service.
	defs = append(defs, cues.Definitions(cueConfig)...)
	// Ring behaviour is per kind: Timer and Alarm share the engine but keep
	// independent live max_ring, volume, ramp, gap and speak_label settings.
	defs = append(defs, ring.Definitions()...)
	// Quiet hours shares the one settings service with the Dashboard and voice
	// Tools; the startup reader above is replaced by this full service.
	defs = append(defs, quiet.Definitions(baseCfg.Quiet)...)
	// Notify's URL and missed-alert switch are live settings; the topic and
	// optional token remain write-only secrets.
	defs = append(defs, notify.Definitions(baseCfg.Notify)...)
	// The model name is a live setting like the wording. Its default comes
	// from the active provider's code default when that provider is built in;
	// otherwise the configured model is both the default and the config layer.
	modelDefault := provider.Model
	if defaultProvider, err := config.Default().Agent.ActiveProvider(); err == nil && config.Default().Agent.Provider == baseCfg.Agent.Provider {
		modelDefault = defaultProvider.Model
	}
	defs = append(defs, agent.ModelDefinition(modelDefault, provider.Model))
	// Voice name and rate are live settings shared with the Dashboard and the
	// voice Tools. The name's config layer is tts.voice; the library adds the
	// custom .safetensors entries once Rescan runs.
	defs = append(defs, voiceSvc.Definitions(voicelib.DefinitionConfig{
		Name: baseCfg.TTS.Voice,
		Rate: baseCfg.Voice.Rate,
	})...)
	settingsSvc, err = settings.New(ctx, st, defs...)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	// Bind after the service exists so the timezone is live overridable: the
	// Dashboard's settings handler and any voice Tool write the same service.
	zone.Bind(settingsSvc)
	// Quiet hours now reads and writes the one live service: a Dashboard edit
	// or a voice Tool write is visible to the next Status read.
	quietSvc.Bind(settingsSvc)
	quietSvc.BindWriter(settingsSvc)
	// Notify now reads the same live settings as the Dashboard and the Tools.
	notifySvc.Bind(settingsSvc)
	// One small worker applies the current Quiet status to the Announcement
	// queue. It reevaluates at the next window boundary or after one second, so
	// a Dashboard edit applies promptly without a request or a second settings
	// observer.
	var quietQueue quiet.HoldQueue
	if announcements != nil {
		quietQueue = announcements
	}
	quietController := quiet.NewController(quietSvc, quietQueue, logger)
	quietController.Start(ctx)
	defer quietController.Close()
	// The matcher reads this per lookup, so a Dashboard change applies without
	// a restart and without a second settings observer.
	nameThreshold.Bind(settingsSvc)
	// Bind the same live settings service the Dashboard writes, so a role
	// change applies on the next Cue. The shared serial sink keeps a Cue from
	// overlapping a reply.
	cues.Bind(settingsSvc, playSink)
	// Bind the voice library to the same live settings service. The first
	// rescan records the TTS revision, smoke-tests copied files, and falls back
	// an unusable active voice before the speaker is bound or a reply starts.
	voiceSvc.Bind(settingsSvc)
	voiceCtx, cancelVoice := context.WithTimeout(ctx, 5*time.Minute)
	var voiceProbe voicelib.Probe
	var voicePreview dashboard.VoicePreview
	if ttsClient != nil {
		voiceProbe = voicelib.NewTTSProbe(ttsClient, cfg.TTS.Timeout.Duration())
		// The browser preview synthesizes one bounded phrase and never opens
		// the audio sink or changes the persistent active voice.
		voicePreview = voicesample.New(ttsClient, cfg.TTS.Timeout.Duration())
	}
	if err := voiceSvc.Rescan(voiceCtx, voiceProbe); err != nil {
		// A library problem must not stop speech: the built-in default still
		// works, and the next rescan can index the files.
		logger.Warn("voice library rescan failed", "error", err.Error())
	}
	if fallback, err := voiceSvc.EnsureActive(voiceCtx); err != nil {
		logger.Warn("voice fallback failed", "error", err.Error())
	} else if fallback != nil && announcements != nil {
		// Exactly one Announcement for the stale active voice. The setting is
		// now the default built-in, so a later rescan or turn cannot repeat it.
		announcements.Enqueue(fmt.Sprintf(
			"The %s voice needs re-exporting for the updated speech model, so I'm using %s.",
			fallback.From, fallback.To))
	}
	if ttsClient != nil {
		// Warm the active voice now; other voices load on first use and stay
		// cached in the TTS process.
		if ref, _, ok := voiceSvc.ActiveSpeech(); ok {
			warmCtx, cancelWarm := context.WithTimeout(voiceCtx, cfg.TTS.Timeout.Duration())
			if err := ttsClient.WarmVoice(warmCtx, ref); err != nil {
				logger.Warn("voice warm-load failed", "voice", ref, "error", err.Error())
			}
			cancelWarm()
		}
	}
	cancelVoice()
	// Every reply, Filler and alert reads the same live voice/rate snapshot.
	if speechSpeaker != nil {
		speechSpeaker.BindVoice(func() speech.VoiceSettings {
			voice, rate := currentSpeechVoice(voiceSvc)
			return speech.VoiceSettings{Voice: voice, Rate: rate}
		})
	}

	// The Timer module installed the Notify missed hook before its recovery,
	// so open/downtime rows already reached ntfy. Only the shutdown wait
	// remains, and Close waits for the final history write.
	if module, ok := toolSet.Module("timer").(*timer.Module); ok {
		if manager := module.Manager(); manager != nil {
			defer manager.Close()
		}
	}
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
		voice, rate := currentSpeechVoice(voiceSvc)
		filler = loadAgentFiller(logger, ttsClient, voice, rate, settingsSvc.String(wording.FillerKey), cfg.TTS.Timeout.Duration())
		synthFiller = func(ctx context.Context, text string) (*speech.Filler, error) {
			ctx, cancel := context.WithTimeout(ctx, cfg.TTS.Timeout.Duration())
			defer cancel()
			voice, rate := currentSpeechVoice(voiceSvc)
			return speech.SynthesizeFiller(ctx, ttsClient, voice, text, wyoming.WithRate(rate))
		}
	}
	// The wording worker keeps the single settings observer. The extra keys and
	// signature make a voice/rate change re-synthesize the cached Filler, so
	// the spoken cue never keeps the old voice forever.
	wordingMgr, err := wording.NewManager(settingsSvc, wordingSetup, filler, synthFiller, logger,
		wording.WithFillerKeys(voicelib.KeyName, voicelib.KeyRate),
		wording.WithFillerSignature(func() string {
			ref, rate, ok := voiceSvc.ActiveSpeech()
			if !ok {
				ref = voiceSvc.DefaultName()
				rate = voiceSvc.Rate()
			}
			return fmt.Sprintf("%s@%.2f", ref, rate)
		}),
	)
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
		// Keep the previous reply and its provenance for follow-ups such as
		// "send that to my phone".
		pipeline.WithLastReply(lastReplyStore),
	}
	// Compute Tool example vectors from the effective Examples (a stored
	// override may already exist). A dead embed service only disables semantic
	// matching, leaving Control phrases and the Agent path working.
	semantic := pipeline.NewSemanticRouter(registry, embedClient, pipeline.SemanticConfig{
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
	// The error Cue follows the same quiet gain as the Unavailable message.
	sessionOpts = append(sessionOpts, reply.WithCueScale(quietSvc.Clip))
	// The Filler is read once per turn from the live source, so a re-synthesized
	// phrase is heard from the next request onward.
	sessionOpts = append(sessionOpts, reply.WithFillerSource(wordingMgr))
	// Replies, Fillers and the error Cue use the absolute quiet volume while
	// the Quiet window is active. The gain is software-only; the media sink
	// volume is never changed and nothing needs restoring.
	sessionOpts = append(sessionOpts, reply.WithVolumeSource(quietSvc.Gain))
	// A speaking reply holds Focus, so a shared "stop" cancels its model stream
	// and playback rather than the stop request itself.
	sessionOpts = append(sessionOpts, reply.WithFocus(focusRegistry))
	// A Ring waits for the current reply sentence, then the reply's next
	// sentence waits for the Ring. With no Ring the gate is a fast no-op.
	sessionOpts = append(sessionOpts, reply.WithSpeechGate(speechGate))
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
		// Keep the typed nil out of the interface so the Sounds page can fall
		// back to synchronous playback on a text-only setup.
		var previewQueue dashboard.PreviewQueue
		if announcements != nil {
			previewQueue = announcements
		}
		dashboardHandler, err := dashboard.New(dashboard.Options{
			Owner:         cfg.Dashboard.Owner,
			Logger:        logger,
			HealthTimeout: cfg.Dashboard.HealthTimeout.Duration(),
			Services:      dashboard.ComposeServiceChecks(cfg),
			Modules:       toolSet,
			Location:      locSvc,
			Settings:      settingsSvc,
			ToolPolicy:    toolPolicy,
			Secrets:       secretsSvc,
			Sounds:        cues,
			PreviewQueue:  previewQueue,
			Voices:        voiceSvc,
			VoiceProbe:    voiceProbe,
			VoicePreview:  voicePreview,
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
		"voices", len(voiceSvc.List()),
		"voice", settingsSvc.String(voicelib.KeyName),
		"voice_rate", settingsSvc.Float(voicelib.KeyRate),
		"voice_revision", voiceSvc.Revision(),
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
func loadAgentFiller(logger *slog.Logger, client *wyoming.Client, voice wyoming.Voice, rate float64, text string, timeout time.Duration) *speech.Filler {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	filler, err := speech.SynthesizeFiller(ctx, client, voice, text, wyoming.WithRate(rate))
	if err != nil {
		logger.Warn("agent filler unavailable", "error", err.Error())
		return nil
	}
	logger.Info("agent filler cached", "text", filler.Text, "audio_bytes", len(filler.PCM))
	return filler
}

// currentSpeechVoice resolves the live active voice and rate. The default
// built-in is the final fallback, so speech never depends on a stale state.
func currentSpeechVoice(service *voicelib.Service) (wyoming.Voice, float64) {
	if service != nil {
		if ref, rate, ok := service.ActiveSpeech(); ok {
			return wyoming.Voice{Name: ref}, rate
		}
		return wyoming.Voice{Name: service.DefaultName()}, service.Rate()
	}
	return wyoming.Voice{}, 1.0
}

// checkTTSService logs whether the TTS service answers describe and voice-info.
// A failure is not fatal: the text reply still works and compose gates startup
// anyway.
func checkTTSService(logger *slog.Logger, client *wyoming.Client, addr string) (wyoming.Info, wyoming.VoiceLibrary) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := client.Describe(ctx)
	if err != nil {
		logger.Warn("tts not ready", "addr", addr, "error", err.Error())
		return wyoming.Info{}, wyoming.VoiceLibrary{}
	}
	voices := 0
	streaming := false
	for _, program := range info.TTS {
		voices += len(program.Voices)
		streaming = streaming || program.SupportsSynthesizeStreaming
	}
	logger.Info("tts ready", "addr", addr, "programs", len(info.TTS), "voices", voices, "streaming", streaming)
	voiceInfo, err := client.VoiceInfo(ctx)
	if err != nil {
		// An older wrapper without the custom events still serves built-ins
		// from describe; only custom-state staleness needs the revision.
		logger.Warn("tts voice-info unavailable", "addr", addr, "error", err.Error())
		return info, wyoming.VoiceLibrary{}
	}
	return info, voiceInfo
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
