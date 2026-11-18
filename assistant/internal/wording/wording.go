// Package wording makes the overridable wording live. Control phrases and
// Agent request prefixes are read once per request from one settings snapshot;
// semantic Examples re-embed in the background and swap in only when ready;
// the Agent Filler is re-synthesized when its text changes; the Unavailable
// message is read on each failure. There is no general hot-reload framework:
// the settings service reports changed keys and one small worker does the slow
// work away from the request path.
package wording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/speech"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// Settings keys for the wording that is not tied to one Tool.
const (
	// PrefixesKey holds the Agent request prefixes. A matching utterance goes
	// straight to the Agent path.
	PrefixesKey = "agent.request_prefixes"
	// FillerKey holds the short phrase spoken while the Agent path waits for
	// its first tokens.
	FillerKey = "agent.filler"
	// UnavailableKey holds the phrase spoken when the Agent path cannot reach
	// its model.
	UnavailableKey = "agent.unavailable_message"
	// ClarifyKey holds the line the model is told to use when a request is
	// addressed to the Assistant but cannot be understood. It is read while the
	// system part is built, so a change applies to the next request.
	ClarifyKey = "agent.clarify_message"
)

// Code defaults. config.yaml is normally the config layer, but a definition
// still needs a value when that layer is absent.
const (
	DefaultFiller             = "One moment."
	DefaultUnavailableMessage = "Sorry, I can't answer that right now."
	DefaultBuildTimeout       = 30 * time.Second
)

// ControlPhrasesKey is one Tool's live Control phrase list.
func ControlPhrasesKey(name string) string { return "tools." + name + ".control_phrases" }

// IntentPhrasesKey is one shared Focus intent's live Control phrase list. An
// intent has no Tool, so its phrases live under the fixed vocabulary rather
// than a tools.<name> section.
func IntentPhrasesKey(intent focus.Intent) string {
	return "focus." + string(intent) + ".phrases"
}

// ExamplesKey is one Tool's live semantic Fast path Examples.
func ExamplesKey(name string) string { return "tools." + name + ".examples" }

// Config is the config.yaml layer for the wording settings.
type Config struct {
	ControlPhrases     map[string]string
	RequestPrefixes    []string
	Filler             string
	UnavailableMessage string
	// ClarifyMessage is optional: an empty value keeps the code default, so a
	// caller that predates the setting still builds.
	ClarifyMessage string
}

// Setup is the wording definition set plus the Tool names each live list
// belongs to. Tools are sorted, as the registry returns them.
type Setup struct {
	Definitions  []settings.Definition
	ControlTools []string
	ExampleTools []string
	FocusIntents []focus.Intent
}

// Build validates the config layer and creates the wording settings. It must
// run before the Tool policy is installed, while registry.Info still returns
// the code declarations.
func Build(reg *tool.Registry, cfg Config) (Setup, error) {
	if reg == nil {
		return Setup{}, fmt.Errorf("wording: registry is required")
	}
	if strings.TrimSpace(cfg.Filler) == "" {
		return Setup{}, fmt.Errorf("wording: agent filler is required")
	}
	if strings.TrimSpace(cfg.UnavailableMessage) == "" {
		return Setup{}, fmt.Errorf("wording: agent unavailable message is required")
	}
	if err := validateNonBlank(cfg.RequestPrefixes); err != nil {
		return Setup{}, fmt.Errorf("wording: %s: %w", PrefixesKey, err)
	}

	names := reg.Names()
	declared := make(map[string]tool.Info, len(names))
	for _, name := range names {
		info, ok := reg.Info(name)
		if !ok {
			return Setup{}, fmt.Errorf("wording: %s disappeared from the registry", name)
		}
		declared[name] = info
	}

	// Group config phrases by owner, applying the same rules the pipeline used
	// to apply at construction: a known Tool, a Speak renderer, no confirm Tool,
	// a known Focus intent, and no phrase claimed by two owners.
	phrasesByTool := make(map[string][]string, len(cfg.ControlPhrases))
	phrasesByIntent := make(map[focus.Intent][]string, len(focus.Intents()))
	owners := make(map[string]string, len(cfg.ControlPhrases))
	for text, target := range cfg.ControlPhrases {
		key := phrase.Normalize(text)
		if key == "" {
			return Setup{}, fmt.Errorf("wording: control phrase %q is empty", text)
		}
		target = strings.TrimSpace(target)
		if target == "" {
			return Setup{}, fmt.Errorf("wording: control phrase %q has no target", text)
		}
		owner := target
		if focus.IsIntentTarget(target) {
			intent, ok := focus.ParseIntent(target)
			if !ok {
				return Setup{}, fmt.Errorf("wording: control phrase %q points at unknown intent %q", text, target)
			}
			owner = IntentPhrasesKey(intent)
			if prev, ok := owners[key]; ok && prev != owner {
				return Setup{}, fmt.Errorf("wording: control phrase %q maps to both %q and %q", text, prev, owner)
			}
			owners[key] = owner
			phrasesByIntent[intent] = append(phrasesByIntent[intent], text)
			continue
		}
		info, ok := declared[target]
		if !ok {
			return Setup{}, fmt.Errorf("wording: control phrase %q points at unknown Tool %q", text, target)
		}
		if !info.HasSpeak {
			return Setup{}, fmt.Errorf("wording: control phrase %q points at %q, which has no Speak renderer", text, target)
		}
		if info.AgentOnly {
			// Control phrases are a Fast-path route; an AgentOnly Tool must
			// only be reachable through a model call.
			return Setup{}, fmt.Errorf("wording: control phrase %q points at AgentOnly tool %q", text, target)
		}
		if info.Permission == tool.PermissionConfirm {
			return Setup{}, fmt.Errorf("wording: control phrase %q points at confirm Tool %q", text, target)
		}
		owner = ControlPhrasesKey(target)
		if prev, ok := owners[key]; ok && prev != owner {
			return Setup{}, fmt.Errorf("wording: control phrase %q maps to both %q and %q", text, prev, owner)
		}
		owners[key] = owner
		phrasesByTool[target] = append(phrasesByTool[target], text)
	}
	// Two spellings that normalize to the same phrase are one Control phrase;
	// keep the first in sorted order so the config layer is valid.
	dedupePhrases(phrasesByTool)
	dedupePhrases(phrasesByIntent)

	setup := Setup{ControlTools: []string{}, ExampleTools: []string{}}
	for _, name := range names {
		info := declared[name]
		// Control phrases are a Fast-path route, so an AgentOnly Tool never gets
		// a live list: the setting must not exist and the Dashboard must not
		// offer an editor for one.
		if info.HasSpeak && !info.AgentOnly && info.Permission != tool.PermissionConfirm {
			options := []settings.Option{
				settings.WithDescription("Exact phrases that route to " + name + ". They apply on the next request."),
				settings.WithValidator(controlPhraseValidator),
			}
			if list := phrasesByTool[name]; len(list) > 0 {
				options = append(options, settings.WithConfig(list))
			}
			setup.Definitions = append(setup.Definitions,
				settings.Strings(ControlPhrasesKey(name), []string{}, options...))
			setup.ControlTools = append(setup.ControlTools, name)
		}
		if pipeline.CanUseFastPath(info) {
			setup.Definitions = append(setup.Definitions, settings.Strings(
				ExamplesKey(name),
				append([]string(nil), info.Examples...),
				settings.WithDescription("Semantic Fast path Examples for "+name+". A change re-embeds in the background; routing keeps the old vectors until the new ones are ready."),
				settings.WithValidator(validateNonBlank),
			))
			setup.ExampleTools = append(setup.ExampleTools, name)
		}
	}

	// Shared Focus intents have no Tool, so each fixed intent gets its own live
	// phrase list. The pipeline routes a phrase to whatever holds Focus; with
	// nothing in Focus a bare "stop" ends the session and the other intents
	// fall through to the semantic Fast path.
	for _, intent := range focus.Intents() {
		options := []settings.Option{
			settings.WithDescription("Exact phrases that act on whatever holds Focus for the " + string(intent) + " intent. They apply on the next request."),
			settings.WithValidator(controlPhraseValidator),
		}
		if list := phrasesByIntent[intent]; len(list) > 0 {
			options = append(options, settings.WithConfig(list))
		}
		setup.Definitions = append(setup.Definitions,
			settings.Strings(IntentPhrasesKey(intent), []string{}, options...))
	}
	setup.FocusIntents = focus.Intents()

	setup.Definitions = append(setup.Definitions,
		settings.Strings(PrefixesKey, []string{},
			settings.WithConfig(append([]string{}, cfg.RequestPrefixes...)),
			settings.WithDescription("Agent request prefixes. A matching utterance goes straight to the Agent path on the next request."),
			settings.WithValidator(validateNonBlank),
		),
		settings.String(FillerKey, DefaultFiller,
			settings.WithConfig(cfg.Filler),
			settings.WithDescription("Short phrase played while the Agent path waits for its first tokens. A change re-synthesizes the cached audio."),
			settings.WithValidator(requireNonBlank),
		),
		settings.String(UnavailableKey, DefaultUnavailableMessage,
			settings.WithConfig(cfg.UnavailableMessage),
			settings.WithDescription("Phrase spoken when the Agent path cannot reach its model. A change applies to the next failure."),
			settings.WithValidator(requireNonBlank),
		),
		clarifyDefinition(cfg.ClarifyMessage),
	)
	return setup, nil
}

// clarifyDefinition builds the live Clarify line. The config layer is optional:
// with no config value the code default (the same line the Agent falls back to)
// is the value, so a setup that never sets the key is unchanged.
func clarifyDefinition(fromConfig string) settings.Definition {
	opts := []settings.Option{
		settings.WithDescription("Line the model is told to use when a request is addressed to the Assistant but cannot be understood. A change applies to the next request."),
		settings.WithValidator(requireNonBlank),
	}
	if text := strings.TrimSpace(fromConfig); text != "" {
		opts = append(opts, settings.WithConfig(text))
	}
	return settings.String(ClarifyKey, agent.DefaultClarifyMessage, opts...)
}

// dedupePhrases sorts each group and drops phrases that normalize to one key,
// so the config layer never contains a duplicate the validator would reject.
func dedupePhrases[K comparable](groups map[K][]string) {
	for key, list := range groups {
		sort.Strings(list)
		unique := list[:0]
		seen := make(map[string]bool, len(list))
		for _, text := range list {
			normalized := phrase.Normalize(text)
			if seen[normalized] {
				continue
			}
			seen[normalized] = true
			unique = append(unique, text)
		}
		groups[key] = unique
	}
}

// controlPhraseValidator rejects blank entries and duplicates within one
// list. A phrase claimed by two owners is dropped at request time rather than
// routing to an arbitrary owner.
func controlPhraseValidator(value any) error {
	list, ok := value.([]string)
	if !ok {
		return fmt.Errorf("want a list of phrases, got %T", value)
	}
	seen := make(map[string]bool, len(list))
	for _, text := range list {
		key := phrase.Normalize(text)
		if key == "" {
			return errors.New("a control phrase must not be blank")
		}
		if seen[key] {
			return fmt.Errorf("control phrase %q is repeated", text)
		}
		seen[key] = true
	}
	return nil
}

// validateNonBlank is the shared list validator for prefixes and Examples.
func validateNonBlank(value any) error {
	list, ok := value.([]string)
	if !ok {
		return fmt.Errorf("want a list of strings, got %T", value)
	}
	for _, text := range list {
		if strings.TrimSpace(text) == "" {
			return errors.New("an entry must not be blank")
		}
	}
	return nil
}

// requireNonBlank is the single-string validator for the Filler and the
// Unavailable message.
func requireNonBlank(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("want a string, got %T", value)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("must not be blank")
	}
	return nil
}

// FillerSynth renders one phrase to cached audio. main supplies the real TTS
// boundary; tests use a fake.
type FillerSynth func(ctx context.Context, text string) (*speech.Filler, error)

// Option configures the live wording worker without changing the constructor's
// shape for existing callers.
type Option func(*Manager)

// WithFillerSignature makes the cached Filler depend on more than its text.
// main passes a voice+rate signature, so selecting a different voice or rate
// re-synthesizes the cue instead of leaving the old voice cached forever.
func WithFillerSignature(signature func() string) Option {
	return func(m *Manager) { m.fillerSignature = signature }
}

// WithFillerKeys names the settings keys that can invalidate the cached Filler
// beyond the Filler text itself. The settings observer is still the single one
// owned by this manager; this only widens what wakes its worker.
func WithFillerKeys(keys ...string) Option {
	return func(m *Manager) { m.fillerKeys = append([]string(nil), keys...) }
}

// WithPersonaPhrases adds the active Persona's extra Control phrases, keyed by
// Tool name. They are claims beside the global lists, never replacements: the
// shipped phrases keep working and a phrase claimed by two owners still falls
// through. The source is read once per request, so a switch applies to the
// next one.
func WithPersonaPhrases(source func() map[string][]string) Option {
	return func(m *Manager) { m.personaPhrases = source }
}

// WithRetryBackoff changes the bounded retry schedule for a failed Examples
// rebuild or Filler synthesis. Production uses the defaults; tests shorten
// them so a retry can be observed without sleeping for seconds.
func WithRetryBackoff(initial, max time.Duration) Option {
	return func(m *Manager) {
		if initial > 0 {
			m.retryInitial = initial
		}
		if max >= initial && max > 0 {
			m.retryMax = max
		}
	}
}

// WithBuildTimeout bounds the time an Examples rebuild can spend in the
// embedder before abandoning the attempt. A stuck embed service gives up its
// turn so subsequent settings changes are not stalled, the previous vectors
// stay live, and the rebuild retries on the backoff schedule.
func WithBuildTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.buildTimeout = d
		}
	}
}

// ErrClosed reports a Manager that has been stopped.
var ErrClosed = errors.New("wording: manager is closed")

// Manager is the live wording seam for the pipeline, the reply session and the
// Agent path. Cheap values are read per use; the worker only rebuilds Examples
// and the cached Filler.
type Manager struct {
	settings     *settings.Service
	controlTools []string
	exampleTools []string
	focusIntents []focus.Intent
	synth        FillerSynth
	logger       *slog.Logger

	// fillerMu guards the immutable cached Filler and the cache-key
	// bookkeeping. The key is the text plus the optional voice/rate signature.
	fillerMu     sync.RWMutex
	filler       *speech.Filler
	fillerKey    string
	attemptedKey string // last key whose synthesis failed

	fillerSignature func() string
	fillerKeys      []string
	personaPhrases  func() map[string][]string

	// retryInitial and retryMax bound the background retry of a failed
	// Examples rebuild or Filler synthesis. The worker owns the timer, so a
	// dead embed or TTS service cannot spin and a recovery needs no restart.
	retryInitial time.Duration
	retryMax     time.Duration
	buildTimeout time.Duration

	// builtMu guards the Examples snapshot the worker last installed and the
	// last build state reported to the Health page. The accessor is in-memory;
	// it never probes the embed service.
	builtMu         sync.Mutex
	built           map[string][]string
	examplesLastErr error
	examplesAt      time.Time // last rebuild attempt, successful or not

	// changeMu serializes a settings change signal with the final swap, so a
	// rebuild whose snapshot was superseded in that last window is discarded
	// instead of being installed.
	changeMu sync.Mutex
	version  uint64
	// examplesDirty records an eligibility change (a Tool's enabled flag or its
	// Permission) since the last successful swap. Such a change leaves every
	// Examples list identical, so the version and the example snapshot alone
	// cannot tell the worker to rebuild; the flag clears only after a swap that
	// was not superseded.
	examplesDirty bool

	// startMu guards the lifecycle fields below.
	startMu sync.Mutex
	router  *pipeline.SemanticRouter
	started bool
	closed  bool
	cancel  context.CancelFunc

	wake    chan struct{}
	barrier chan chan struct{}
	done    chan struct{}
}

// NewManager builds the live wording source. initial is the Filler cached at
// startup, if any; synth may be nil when TTS is not configured.
func NewManager(svc *settings.Service, setup Setup, initial *speech.Filler, synth FillerSynth, logger *slog.Logger, opts ...Option) (*Manager, error) {
	if svc == nil {
		return nil, fmt.Errorf("wording: settings service is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		settings:     svc,
		controlTools: append([]string(nil), setup.ControlTools...),
		exampleTools: append([]string(nil), setup.ExampleTools...),
		focusIntents: append([]focus.Intent(nil), setup.FocusIntents...),
		synth:        synth,
		logger:       logger,
		filler:       initial,
		retryInitial: 2 * time.Second,
		retryMax:     2 * time.Minute,
		buildTimeout: DefaultBuildTimeout,
		wake:         make(chan struct{}, 1),
		barrier:      make(chan chan struct{}),
		done:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	if initial != nil {
		text := initial.Text
		if m.settings != nil {
			if s := m.settings.String(FillerKey); s != "" {
				text = s
			}
		}
		m.fillerKey = m.keyFor(text)
	}
	return m, nil
}

// keyFor builds the cache key from the phrase, any configured extra filler keys,
// and, when configured, the live voice/rate signature. A voice or pronunciation
// change therefore invalidates the cached audio even though the Filler text is
// unchanged.
func (m *Manager) keyFor(text string) string {
	key := strings.TrimSpace(text)
	if m.fillerSignature != nil {
		key += "\x00" + m.fillerSignature()
	}
	if m.settings != nil && len(m.fillerKeys) > 0 {
		for _, k := range m.fillerKeys {
			if k == "voice.name" || k == "voice.rate" {
				continue
			}
			if v, ok := m.settings.Get(k); ok {
				key += fmt.Sprintf("\x00%s=%v", k, v.Value)
			}
		}
	}
	return key
}

// Start installs the settings observer and runs the background worker. router
// may be nil when semantic routing is unavailable; the Filler still reloads.
// Calling Start more than once, or after Close, is a no-op.
func (m *Manager) Start(ctx context.Context, router *pipeline.SemanticRouter) {
	if m == nil {
		return
	}
	m.startMu.Lock()
	if m.started || m.closed {
		m.startMu.Unlock()
		return
	}
	m.started = true
	m.router = router
	m.settings.SetObserver(m.Changed)
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.startMu.Unlock()
	go m.run(runCtx)
	// Run one sync now: it installs nothing when the startup build already
	// succeeded, and it starts the first retry when that build failed.
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Close stops the worker and removes the settings observer. It is safe to call
// more than once and from another goroutine.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.startMu.Lock()
	if m.closed {
		m.startMu.Unlock()
		return
	}
	m.closed = true
	started, cancel := m.started, m.cancel
	m.settings.SetObserver(nil)
	m.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		<-m.done
	}
}

// Changed receives the settings keys a write touched. It only wakes the worker;
// the worker reads the current effective values itself, so a burst of writes
// collapses into one rebuild.
func (m *Manager) Changed(keys []string) {
	if m == nil || !m.relevant(keys) {
		return
	}
	m.changeMu.Lock()
	m.version++
	if eligibilityChanged(keys) {
		m.examplesDirty = true
	}
	m.changeMu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// relevant reports whether a changed key can affect background work. Control
// phrases and the Unavailable message are read per request and need none.
func (m *Manager) relevant(keys []string) bool {
	for _, key := range keys {
		if key == FillerKey {
			return true
		}
		for _, extra := range m.fillerKeys {
			if key == extra {
				return true
			}
		}
		// A Tool's Examples feed the router, and its enabled/Permission state
		// changes which Tools are eligible. Both wake the worker; Changed also
		// marks an eligibility change as a pending rebuild, because that leaves
		// the Examples text identical.
		if strings.HasPrefix(key, "tools.") {
			return true
		}
	}
	return false
}

// eligibilityChanged reports whether any key changes a Tool's Fast-path
// eligibility rather than its Examples text. The registry reads both the
// enabled flag and the Permission on every build, so the index must rebuild
// even when no Example changed.
func eligibilityChanged(keys []string) bool {
	for _, key := range keys {
		if strings.HasSuffix(key, ".enabled") || strings.HasSuffix(key, ".permission") {
			return true
		}
	}
	return false
}

// Wait blocks until the worker has applied every change known when it reaches
// the barrier. Tests use it instead of sleeping.
func (m *Manager) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.startMu.Lock()
	started := m.started
	m.startMu.Unlock()
	if !started {
		return nil
	}
	reply := make(chan struct{})
	select {
	case m.barrier <- reply:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return ErrClosed
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return ErrClosed
	}
}

// run is the one background goroutine. All rebuilds are serialized here, so a
// stale build can never overwrite a newer one and there is no background data
// race with request reads. A failed rebuild schedules one bounded-backoff
// retry; a later change signal or success cancels it.
func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	backoff := m.retryInitial
	var timer *time.Timer
	var retry <-chan time.Time
	stopRetry := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		retry = nil
	}
	defer stopRetry()
	scheduleRetry := func() {
		stopRetry()
		if backoff <= 0 {
			return
		}
		timer = time.NewTimer(backoff)
		retry = timer.C
		if next := backoff * 2; next < m.retryMax {
			backoff = next
		} else if m.retryMax > 0 {
			backoff = m.retryMax
		}
	}
	applied := func(failed bool) {
		if failed {
			scheduleRetry()
			return
		}
		stopRetry()
		backoff = m.retryInitial
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			applied(m.sync(ctx, false))
		case reply := <-m.barrier:
			applied(m.sync(ctx, false))
			close(reply)
		case <-retry:
			retry = nil
			if m.sync(ctx, true) {
				scheduleRetry()
			} else {
				stopRetry()
				backoff = m.retryInitial
			}
		}
	}
}

// sync applies the current settings. It is the only writer of the cached
// Filler and the installed Examples snapshot. It reports whether a background
// retry is still needed.
func (m *Manager) sync(ctx context.Context, retry bool) bool {
	fillerFailed := m.syncFiller(ctx, retry)
	examplesFailed := m.syncExamples(ctx)
	return fillerFailed || examplesFailed
}

// Routing returns the live Control phrase table and Agent request prefixes from
// one settings snapshot. Normalized phrases claimed by two Tools are dropped,
// so ambiguity falls through to the semantic or Agent path instead of choosing
// an owner at random.
func (m *Manager) Routing() pipeline.RoutingWords {
	keys := make([]string, 0, len(m.controlTools)+len(m.focusIntents)+1)
	keys = append(keys, PrefixesKey)
	for _, name := range m.controlTools {
		keys = append(keys, ControlPhrasesKey(name))
	}
	for _, intent := range m.focusIntents {
		keys = append(keys, IntentPhrasesKey(intent))
	}
	values := m.settings.GetMany(keys...)

	claims := make(map[string][]string)
	for _, name := range m.controlTools {
		value, ok := values[ControlPhrasesKey(name)]
		if !ok {
			continue
		}
		list, _ := value.Value.([]string)
		for _, text := range list {
			key := phrase.Normalize(text)
			if key == "" {
				continue
			}
			if !contains(claims[key], name) {
				claims[key] = append(claims[key], name)
			}
		}
	}
	for _, intent := range m.focusIntents {
		value, ok := values[IntentPhrasesKey(intent)]
		if !ok {
			continue
		}
		list, _ := value.Value.([]string)
		for _, text := range list {
			key := phrase.Normalize(text)
			if key == "" {
				continue
			}
			owner := "intent:" + string(intent)
			if !contains(claims[key], owner) {
				claims[key] = append(claims[key], owner)
			}
		}
	}
	// The active Persona's trigger phrases are extra claims, never a
	// replacement. They are applied after the global table below, so a phrase
	// the owner adds to a global Control phrase later still keeps routing
	// there; the Persona phrase simply does not win.
	var personaClaims map[string][]string
	if m.personaPhrases != nil {
		personaClaims = make(map[string][]string)
		for name, list := range m.personaPhrases() {
			for _, text := range list {
				key := phrase.Normalize(text)
				if key == "" {
					continue
				}
				if len(claims[key]) > 0 {
					continue
				}
				if !contains(personaClaims[key], name) {
					personaClaims[key] = append(personaClaims[key], name)
				}
			}
		}
	}
	phrases := make(map[string]string, len(claims)+len(personaClaims))
	intentPhrases := make(map[string]string, len(claims))
	for key, owners := range claims {
		if len(owners) != 1 {
			// A phrase claimed by two owners falls through instead of picking
			// one at random.
			continue
		}
		owner := owners[0]
		if intent, ok := strings.CutPrefix(owner, "intent:"); ok {
			intentPhrases[key] = intent
			continue
		}
		phrases[key] = owner
	}
	for key, owners := range personaClaims {
		if len(owners) != 1 {
			// Two Persona Tools claiming one phrase are ambiguous too.
			continue
		}
		phrases[key] = owners[0]
	}

	words := pipeline.RoutingWords{ControlPhrases: phrases, IntentPhrases: intentPhrases}
	if value, ok := values[PrefixesKey]; ok {
		if list, ok := value.Value.([]string); ok {
			words.RequestPrefixes = list
		}
	}
	return words
}

// Examples returns the effective Examples for every Tool that has the setting.
// One GetMany call gives a coherent snapshot for a rebuild.
func (m *Manager) Examples() map[string][]string {
	keys := make([]string, len(m.exampleTools))
	for i, name := range m.exampleTools {
		keys[i] = ExamplesKey(name)
	}
	values := m.settings.GetMany(keys...)
	out := make(map[string][]string, len(m.exampleTools))
	for _, name := range m.exampleTools {
		if value, ok := values[ExamplesKey(name)]; ok {
			if list, ok := value.Value.([]string); ok {
				out[name] = list
			}
		}
	}
	return out
}

// MarkExamplesBuilt records the Examples snapshot a successful startup build
// installed and clears any earlier build error. Start must not have run yet.
func (m *Manager) MarkExamplesBuilt(examples map[string][]string) {
	if m == nil {
		return
	}
	m.builtMu.Lock()
	m.built = cloneExamples(examples)
	m.examplesLastErr = nil
	m.examplesAt = time.Now()
	m.builtMu.Unlock()
}

// MarkExamplesFailed records a failed startup build so the Health page reports
// the error and the attempt time while the worker keeps retrying. The router
// stays attached: Control phrases and the Agent path are unaffected.
func (m *Manager) MarkExamplesFailed(err error) {
	if m == nil {
		return
	}
	m.builtMu.Lock()
	m.examplesLastErr = err
	m.examplesAt = time.Now()
	m.builtMu.Unlock()
}

// ExamplesState reports the semantic index's in-memory state for the Health
// page: the number of indexed Tools, the last rebuild error (nil after a
// successful swap) and the time of the last rebuild attempt. A nil manager or
// router reports zero Tools and no attempt. It returns no Tool output or
// settings values and adds no service probe.
func (m *Manager) ExamplesState() (tools int, lastErr error, lastAttempt time.Time) {
	if m == nil {
		return 0, nil, time.Time{}
	}
	m.startMu.Lock()
	router := m.router
	m.startMu.Unlock()
	m.builtMu.Lock()
	lastErr, lastAttempt = m.examplesLastErr, m.examplesAt
	m.builtMu.Unlock()
	return router.ToolCount(), lastErr, lastAttempt
}

// UnavailableMessage returns the phrase the Agent path says when the model is
// unreachable. It is read per failure, so a change applies to the next one.
func (m *Manager) UnavailableMessage() string {
	if m == nil {
		return DefaultUnavailableMessage
	}
	text := strings.TrimSpace(m.settings.String(UnavailableKey))
	if text == "" {
		return DefaultUnavailableMessage
	}
	return text
}

// ClarifyMessage returns the line the prompt asks for when a request is
// addressed to the Assistant but unclear. It is read while the system part is
// built, so a change applies to the next request without a restart.
func (m *Manager) ClarifyMessage() string {
	if m == nil {
		return agent.DefaultClarifyMessage
	}
	text := strings.TrimSpace(m.settings.String(ClarifyKey))
	if text == "" {
		return agent.DefaultClarifyMessage
	}
	return text
}

// Filler returns the cached Filler for this request. It never blocks on TTS;
// a text change is picked up by the worker and the old audio keeps serving
// until the replacement is ready.
func (m *Manager) Filler() *speech.Filler {
	if m == nil {
		return nil
	}
	m.fillerMu.RLock()
	defer m.fillerMu.RUnlock()
	return m.filler
}

// syncExamples rebuilds the index off to the side. Old vectors stay live on a
// failure, and a set whose snapshot was superseded while embedding is
// discarded instead of being swapped in.
func (m *Manager) syncExamples(ctx context.Context) bool {
	if m.router == nil {
		return false
	}
	m.changeMu.Lock()
	startVersion := m.version
	// An eligibility change (enabled or Permission) rebuilds even when every
	// Examples list is identical, because the registry decides which Tools may
	// enter the index at build time.
	eligibilityDirty := m.examplesDirty
	m.changeMu.Unlock()

	current := m.Examples()
	m.builtMu.Lock()
	built := cloneExamples(m.built)
	m.builtMu.Unlock()
	if !eligibilityDirty && equalExamples(current, built) {
		return false
	}
	// Bound the rebuild to m.buildTimeout so a stuck embed service does not
	// block later settings changes. On timeout, the existing vectors remain
	// live and the worker starts a bounded retry.
	buildCtx, cancel := context.WithTimeout(ctx, m.buildTimeout)
	set, err := m.router.BuildSet(buildCtx, current)
	cancel()
	if err != nil {
		m.logger.Warn("wording: examples rebuild failed, keeping old vectors", "error", err.Error())
		// The flag stays set, so the bounded retry keeps trying until a rebuild
		// succeeds. Keep the error visible to the Health page; the live index
		// (if any) is untouched.
		m.builtMu.Lock()
		m.examplesLastErr = err
		m.examplesAt = time.Now()
		m.builtMu.Unlock()
		return true
	}
	// Check and swap are atomic with respect to a change signal. A newer
	// override that arrived at any point up to here makes this set stale; the
	// signal that accompanies it guarantees another rebuild.
	m.changeMu.Lock()
	if m.version != startVersion {
		m.changeMu.Unlock()
		m.logger.Info("wording: examples rebuild superseded, discarding stale vectors")
		return false
	}
	m.router.Swap(set)
	m.builtMu.Lock()
	m.built = current
	m.examplesLastErr = nil
	m.examplesAt = time.Now()
	m.builtMu.Unlock()
	// The installed set already reflects this snapshot's eligibility, so the
	// flag clears only here: after a swap that was not superseded.
	m.examplesDirty = false
	m.changeMu.Unlock()
	m.logger.Info("wording: examples rebuilt", "tools", set.ToolCount())
	return false
}

// syncFiller re-synthesizes the cached audio when the effective Filler text
// changed. A failed synthesis keeps the old audio; the worker retries the same
// key on its bounded backoff, so a dead TTS service cannot spin and a transient
// failure recovers without another wording change. The manager applies no
// timeout of its own here: production's synthFiller already wraps TTS in
// cfg.TTS.Timeout, and tests supply their own bounded fakes.
func (m *Manager) syncFiller(ctx context.Context, retry bool) bool {
	text := strings.TrimSpace(m.settings.String(FillerKey))
	if text == "" || m.synth == nil {
		return false
	}
	key := m.keyFor(text)
	m.fillerMu.RLock()
	cached, cachedKey, attemptedKey := m.filler, m.fillerKey, m.attemptedKey
	m.fillerMu.RUnlock()
	if cached != nil && cachedKey == key {
		return false
	}
	// A normal wake never re-attempts a key that just failed; only the retry
	// tick does, so a burst of unrelated settings cannot hammer TTS. It must
	// still keep the pending retry alive: the cached audio, if any, belongs to
	// a different key (the equal-cache case returned above).
	if attemptedKey == key && !retry {
		return true
	}
	filler, err := m.synth(ctx, text)
	if err != nil {
		m.logger.Warn("wording: filler re-synthesis failed, keeping old audio", "error", err.Error())
		m.fillerMu.Lock()
		m.attemptedKey = key
		m.fillerMu.Unlock()
		return true
	}
	if filler == nil || len(filler.PCM) == 0 {
		m.logger.Warn("wording: filler synthesis returned no audio, keeping old audio")
		m.fillerMu.Lock()
		m.attemptedKey = key
		m.fillerMu.Unlock()
		return true
	}
	m.fillerMu.Lock()
	m.filler = filler
	m.fillerKey = key
	m.attemptedKey = ""
	m.fillerMu.Unlock()
	m.logger.Info("wording: filler re-synthesized", "text", filler.Text, "audio_bytes", len(filler.PCM))
	return false
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// cloneExamples copies a snapshot so neither the stored nor the compared map
// can be mutated by a caller.
func cloneExamples(examples map[string][]string) map[string][]string {
	if examples == nil {
		return nil
	}
	out := make(map[string][]string, len(examples))
	for name, list := range examples {
		out[name] = append([]string(nil), list...)
	}
	return out
}

// equalExamples compares two snapshots. An absent entry and an empty list are
// the same for routing.
func equalExamples(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, list := range a {
		other, ok := b[name]
		if !ok || len(list) != len(other) {
			return false
		}
		for i := range list {
			if list[i] != other[i] {
				return false
			}
		}
	}
	return true
}
