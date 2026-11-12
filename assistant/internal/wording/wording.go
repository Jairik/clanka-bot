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
)

// Code defaults. config.yaml is normally the config layer, but a definition
// still needs a value when that layer is absent.
const (
	DefaultFiller             = "One moment."
	DefaultUnavailableMessage = "Sorry, I can't answer that right now."
)

// ControlPhrasesKey is one Tool's live Control phrase list.
func ControlPhrasesKey(name string) string { return "tools." + name + ".control_phrases" }

// ExamplesKey is one Tool's live semantic Fast path Examples.
func ExamplesKey(name string) string { return "tools." + name + ".examples" }

// Config is the config.yaml layer for the wording settings.
type Config struct {
	ControlPhrases     map[string]string
	RequestPrefixes    []string
	Filler             string
	UnavailableMessage string
}

// Setup is the wording definition set plus the Tool names each live list
// belongs to. Tools are sorted, as the registry returns them.
type Setup struct {
	Definitions  []settings.Definition
	ControlTools []string
	ExampleTools []string
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

	// Group config phrases by Tool, applying the same rules the pipeline used
	// to apply at construction: a known Tool, a Speak renderer, no confirm Tool
	// and no phrase claimed by two Tools.
	phrasesByTool := make(map[string][]string, len(cfg.ControlPhrases))
	owners := make(map[string]string, len(cfg.ControlPhrases))
	for text, name := range cfg.ControlPhrases {
		key := phrase.Normalize(text)
		if key == "" {
			return Setup{}, fmt.Errorf("wording: control phrase %q is empty", text)
		}
		info, ok := declared[name]
		if !ok {
			return Setup{}, fmt.Errorf("wording: control phrase %q points at unknown Tool %q", text, name)
		}
		if !info.HasSpeak {
			return Setup{}, fmt.Errorf("wording: control phrase %q points at %q, which has no Speak renderer", text, name)
		}
		if info.Permission == tool.PermissionConfirm {
			return Setup{}, fmt.Errorf("wording: control phrase %q points at confirm Tool %q", text, name)
		}
		if prev, ok := owners[key]; ok && prev != name {
			return Setup{}, fmt.Errorf("wording: control phrase %q maps to both %q and %q", text, prev, name)
		}
		owners[key] = name
		phrasesByTool[name] = append(phrasesByTool[name], text)
	}
	for name, list := range phrasesByTool {
		sort.Strings(list)
		// Two spellings that normalize to the same phrase are one Control
		// phrase; keep the first in sorted order so the config layer is valid.
		unique := list[:0]
		seen := make(map[string]bool, len(list))
		for _, text := range list {
			key := phrase.Normalize(text)
			if seen[key] {
				continue
			}
			seen[key] = true
			unique = append(unique, text)
		}
		phrasesByTool[name] = unique
	}

	setup := Setup{ControlTools: []string{}, ExampleTools: []string{}}
	for _, name := range names {
		info := declared[name]
		if info.HasSpeak && info.Permission != tool.PermissionConfirm {
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
	)
	return setup, nil
}

// controlPhraseValidator rejects blank entries and duplicates within one
// Tool's list. A phrase claimed by two Tools is dropped at request time rather
// than routing to an arbitrary owner.
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

// ErrClosed reports a Manager that has been stopped.
var ErrClosed = errors.New("wording: manager is closed")

// Manager is the live wording seam for the pipeline, the reply session and the
// Agent path. Cheap values are read per use; the worker only rebuilds Examples
// and the cached Filler.
type Manager struct {
	settings     *settings.Service
	controlTools []string
	exampleTools []string
	synth        FillerSynth
	logger       *slog.Logger

	// fillerMu guards the immutable cached Filler and the text bookkeeping.
	fillerMu  sync.RWMutex
	filler    *speech.Filler
	fillerTxt string
	attempted string // last text whose synthesis failed

	// builtMu guards the Examples snapshot the worker last installed.
	builtMu sync.Mutex
	built   map[string][]string

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
func NewManager(svc *settings.Service, setup Setup, initial *speech.Filler, synth FillerSynth, logger *slog.Logger) (*Manager, error) {
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
		synth:        synth,
		logger:       logger,
		filler:       initial,
		wake:         make(chan struct{}, 1),
		barrier:      make(chan chan struct{}),
		done:         make(chan struct{}),
	}
	if initial != nil {
		m.fillerTxt = strings.TrimSpace(initial.Text)
	}
	return m, nil
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
		// A Tool's Examples feed the router, and its enabled/Permission state
		// changes which Tools are eligible. Rebuilding on either is cheap and
		// keeps the index fail-closed.
		if strings.HasPrefix(key, "tools.") {
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
// race with request reads.
func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			m.sync(ctx)
		case reply := <-m.barrier:
			m.sync(ctx)
			close(reply)
		}
	}
}

// sync applies the current settings. It is the only writer of the cached
// Filler and the installed Examples snapshot.
func (m *Manager) sync(ctx context.Context) {
	m.syncFiller(ctx)
	m.syncExamples(ctx)
}

// Routing returns the live Control phrase table and Agent request prefixes from
// one settings snapshot. Normalized phrases claimed by two Tools are dropped,
// so ambiguity falls through to the semantic or Agent path instead of choosing
// an owner at random.
func (m *Manager) Routing() pipeline.RoutingWords {
	keys := make([]string, 0, len(m.controlTools)+1)
	keys = append(keys, PrefixesKey)
	for _, name := range m.controlTools {
		keys = append(keys, ControlPhrasesKey(name))
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
	phrases := make(map[string]string, len(claims))
	for key, owners := range claims {
		if len(owners) == 1 {
			phrases[key] = owners[0]
		}
	}

	words := pipeline.RoutingWords{ControlPhrases: phrases}
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
// installed. Start must not have run yet.
func (m *Manager) MarkExamplesBuilt(examples map[string][]string) {
	if m == nil {
		return
	}
	m.builtMu.Lock()
	m.built = cloneExamples(examples)
	m.builtMu.Unlock()
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
func (m *Manager) syncExamples(ctx context.Context) {
	if m.router == nil {
		return
	}
	current := m.Examples()
	m.builtMu.Lock()
	built := cloneExamples(m.built)
	m.builtMu.Unlock()
	if equalExamples(current, built) {
		return
	}
	set, err := m.router.BuildSet(ctx, current)
	if err != nil {
		m.logger.Warn("wording: examples rebuild failed, keeping old vectors", "error", err.Error())
		return
	}
	if !equalExamples(current, m.Examples()) {
		m.logger.Info("wording: examples rebuild superseded, discarding stale vectors")
		return
	}
	m.router.Swap(set)
	m.builtMu.Lock()
	m.built = current
	m.builtMu.Unlock()
	m.logger.Info("wording: examples rebuilt", "tools", set.ToolCount())
}

// syncFiller re-synthesizes the cached audio when the effective Filler text
// changed. A failed synthesis keeps the old audio and is not retried until the
// text changes again, so a dead TTS service cannot spin.
func (m *Manager) syncFiller(ctx context.Context) {
	text := strings.TrimSpace(m.settings.String(FillerKey))
	if text == "" || m.synth == nil {
		return
	}
	m.fillerMu.RLock()
	cached, cachedText, attempted := m.filler, m.fillerTxt, m.attempted
	m.fillerMu.RUnlock()
	if cached != nil && cachedText == text {
		return
	}
	if attempted == text {
		return
	}
	filler, err := m.synth(ctx, text)
	if err != nil {
		m.logger.Warn("wording: filler re-synthesis failed, keeping old audio", "error", err.Error())
		m.fillerMu.Lock()
		m.attempted = text
		m.fillerMu.Unlock()
		return
	}
	if filler == nil || len(filler.PCM) == 0 {
		m.logger.Warn("wording: filler synthesis returned no audio, keeping old audio")
		m.fillerMu.Lock()
		m.attempted = text
		m.fillerMu.Unlock()
		return
	}
	m.fillerMu.Lock()
	m.filler = filler
	m.fillerTxt = filler.Text
	m.attempted = ""
	m.fillerMu.Unlock()
	m.logger.Info("wording: filler re-synthesized", "text", filler.Text, "audio_bytes", len(filler.PCM))
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
