// Package conformance is the shared contract test for Tool modules. A module's
// own test calls Run once; the helper checks registration, Local-only,
// Permission, config defaults, Speak and Example routing against the real
// semantic Fast path.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools"
	"gopkg.in/yaml.v3"
)

// EmbedURLEnv names the real MiniLM service. It is the same variable the
// routing eval uses, so CI sets one value for every real-model check.
const EmbedURLEnv = "ROUTING_EVAL_EMBED_URL"

// Sampler is implemented by a module with Speak Tools. It returns one
// representative successful output per canonical Tool name, so the suite can
// exercise the real Speak renderer without running a handler or touching the
// outside world.
type Sampler interface {
	ConformanceSamples() map[string]any
}

// Run checks one Tool module. Call it once from the module's own test package:
//
//	func TestModuleConformance(t *testing.T) { conformance.Run(t, NewModule()) }
//
// The static checks always run. The Example routing check embeds against the
// real MiniLM service in ROUTING_EVAL_EMBED_URL and skips without one, exactly
// like the routing eval.
func Run(t *testing.T, module tool.Module) {
	t.Helper()
	name := strings.TrimSpace(module.Name())
	if name == "" {
		t.Fatal("module Name() is empty")
	}
	t.Run(name, func(t *testing.T) {
		checked := inspect(module)
		for _, problem := range checked.problems {
			t.Error(problem)
		}
		// Routing needs every Tool registered and every static invariant met.
		if t.Failed() {
			return
		}
		t.Run("Examples route on the semantic Fast path", func(t *testing.T) {
			endpoint := strings.TrimSpace(os.Getenv(EmbedURLEnv))
			if endpoint == "" {
				t.Skipf("set %s to the real MiniLM embed service to check Example routing", EmbedURLEnv)
			}
			tuning, err := loadShippedTuning()
			if err != nil {
				t.Fatalf("load shipped semantic tuning: %v", err)
			}
			problems, err := routingProblems(checked.registry, checked.tools, endpoint, tuning)
			if err != nil {
				t.Fatalf("semantic router: %v", err)
			}
			for _, problem := range problems {
				t.Error(problem)
			}
		})
	})
}

// inspection is one module registered with the production module list.
type inspection struct {
	registry *tool.Registry
	// tools are the canonical names the module under test added.
	tools    []string
	problems []string
}

// inspect registers the production module set with the module under test in
// place of its module-list entry, then checks every static contract for the
// module's own Tools. It never calls a handler or a health check.
func inspect(module tool.Module) inspection {
	result := inspection{registry: tool.NewRegistry(5 * time.Second)}
	deps := testDeps()
	for _, candidate := range productionModules(module) {
		before := nameSet(result.registry)
		if configurable, ok := candidate.(tool.Configurable); ok {
			// A nil section must decode over the code defaults and validate.
			if err := configurable.Configure(nil); err != nil {
				result.problems = append(result.problems, fmt.Sprintf("module %s: config defaults do not validate: %v", candidate.Name(), err))
				continue
			}
		}
		if err := candidate.Register(result.registry, deps); err != nil {
			result.problems = append(result.problems, fmt.Sprintf("module %s: does not register cleanly: %v", candidate.Name(), err))
			continue
		}
		if candidate.Name() == module.Name() {
			result.tools = addedNames(result.registry, before)
		}
	}
	if len(result.tools) == 0 {
		result.problems = append(result.problems, fmt.Sprintf("module %s: registered no Tools", module.Name()))
	}
	result.problems = append(result.problems, metadataProblems(module, result.registry, result.tools)...)
	result.problems = append(result.problems, secretProblems(module)...)
	result.problems = append(result.problems, intentProblems(module)...)
	return result
}

// productionModules returns the one module list with the module under test in
// its own slot. A generated module not yet in the list is appended.
func productionModules(module tool.Module) []tool.Module {
	modules := tools.NewSet().Modules()
	for i, candidate := range modules {
		if candidate.Name() == module.Name() {
			modules[i] = module
			return modules
		}
	}
	return append(modules, module)
}

// secretProblems checks a module's required-secret declarations. The seam is
// optional, but a declaration that is set must be usable as both a store key
// and an environment variable name.
func secretProblems(module tool.Module) []string {
	declarer, ok := module.(tool.SecretDeclarer)
	if !ok {
		return nil
	}
	var problems []string
	seen := make(map[string]bool)
	for i, raw := range declarer.RequiredSecrets() {
		name := strings.TrimSpace(raw)
		if name == "" {
			problems = append(problems, fmt.Sprintf("module %s: RequiredSecrets[%d] is empty", module.Name(), i))
			continue
		}
		if name != raw {
			problems = append(problems, fmt.Sprintf("module %s: RequiredSecrets[%d] %q has surrounding whitespace", module.Name(), i, raw))
		}
		if seen[name] {
			problems = append(problems, fmt.Sprintf("module %s: RequiredSecrets repeats %q", module.Name(), name))
			continue
		}
		seen[name] = true
	}
	return problems
}

// intentProblems checks a module's optional Focus-intent declaration. The seam
// is optional, but a declaration that is set must name only real intents, with
// no blanks or repeats, and must not be empty.
func intentProblems(module tool.Module) []string {
	declarer, ok := module.(tool.IntentDeclarer)
	if !ok {
		return nil
	}
	declared := declarer.Intents()
	var problems []string
	if len(declared) == 0 {
		return []string{fmt.Sprintf("module %s: Intents() declares no Focus intents; omit the interface or name the intents it handles", module.Name())}
	}
	seen := make(map[focus.Intent]bool, len(declared))
	for i, intent := range declared {
		if _, ok := focus.ParseIntent(string(intent)); !ok {
			problems = append(problems, fmt.Sprintf("module %s: Intents[%d] %q is not a Focus intent", module.Name(), i, intent))
			continue
		}
		if seen[intent] {
			problems = append(problems, fmt.Sprintf("module %s: Intents repeats %q", module.Name(), intent))
			continue
		}
		seen[intent] = true
	}
	return problems
}

// metadataProblems checks the declarations and renderers of the module's Tools.
// Registration already rejects an unset Local-only flag or an unknown
// Permission; those failures arrive through the module's register problem.
func metadataProblems(module tool.Module, registry *tool.Registry, names []string) []string {
	var problems []string

	var samples map[string]any
	sampler, hasSampler := module.(Sampler)
	if hasSampler {
		samples = sampler.ConformanceSamples()
	}

	hasSpeak := make([]string, 0, len(names))
	for _, name := range names {
		info, ok := registry.Info(name)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: disappeared from the registry after registration", name))
			continue
		}
		if info.Permission != tool.PermissionRead && info.Permission != tool.PermissionAct && info.Permission != tool.PermissionConfirm {
			problems = append(problems, fmt.Sprintf("%s: invalid Permission %q (want read, act or confirm)", name, info.Permission))
		}
		if info.HasSpeak {
			hasSpeak = append(hasSpeak, name)
			if hasSampler {
				sample, ok := samples[name]
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: add a representative output to ConformanceSamples so Speak can be checked", name))
				} else if err := checkSpeak(registry, name, sample); err != nil {
					problems = append(problems, err.Error())
				}
			}
		}
		if len(info.Examples) == 0 {
			if info.AgentOnly {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: is not on the semantic Fast path and is not declared AgentOnly; add Speak + Examples (+ Extract for arguments), or set AgentOnly", name))
			continue
		}
		if info.AgentOnly {
			problems = append(problems, fmt.Sprintf("%s: AgentOnly must not declare Examples; remove them or drop AgentOnly", name))
			continue
		}
		if !pipeline.FastPathEligible(info) {
			problems = append(problems, fmt.Sprintf("%s: declares Examples but is not on the semantic Fast path (needs read/act Permission, Speak, and no required arguments without Extract)", name))
		}
		for i, example := range info.Examples {
			if strings.TrimSpace(example) == "" {
				problems = append(problems, fmt.Sprintf("%s: Examples[%d] is empty", name, i))
			}
		}
	}
	if len(hasSpeak) > 0 && !hasSampler {
		problems = append(problems, fmt.Sprintf("module %s: implement ConformanceSamples() map[string]any with a representative output for %s", module.Name(), strings.Join(hasSpeak, ", ")))
	}
	for _, name := range sortedKeys(samples) {
		if !contains(names, name) {
			problems = append(problems, fmt.Sprintf("module %s: ConformanceSamples names %q, which it does not register", module.Name(), name))
		}
	}
	return problems
}

// checkSpeak renders one sample and reports an empty, missing or panicking
// Speak instead of letting the test binary die.
func checkSpeak(registry *tool.Registry, name string, sample any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s: Speak panicked for its sample output: %v", name, recovered)
		}
	}()
	text, ok := registry.Speak(name, sample)
	if !ok {
		return fmt.Errorf("%s: Speak returned no text for its sample output (check the sample's type)", name)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%s: Speak returned empty text for its sample output", name)
	}
	return nil
}

// routingProblems embeds every Example against the real MiniLM service and
// checks that the semantic router returns its own Tool. An Example that ties
// with another Tool lands below the margin and fails here.
func routingProblems(registry *tool.Registry, names []string, endpoint string, tuning semanticTuning) ([]string, error) {
	router := pipeline.NewSemanticRouter(registry, embed.NewClient(endpoint, tuning.Model), pipeline.SemanticConfig{
		Threshold: tuning.Threshold,
		Margin:    tuning.Margin,
		Joiners:   tuning.Joiners,
		// Routing quality is under test, not latency (#27); a slow CPU embed
		// call must not turn into a false routing failure.
		Timeout: 10 * time.Second,
	})
	ctx := context.Background()
	if err := router.Build(ctx); err != nil {
		return nil, fmt.Errorf("build router against %s: %w", endpoint, err)
	}

	var problems []string
	for _, name := range names {
		info, ok := registry.Info(name)
		if !ok || !pipeline.FastPathEligible(info) {
			// Agent-only Tools have no Examples by contract; skip them rather
			// than inventing a route the semantic path must never take.
			continue
		}
		for _, example := range info.Examples {
			if strings.TrimSpace(example) == "" {
				continue // the static check already names the empty Example
			}
			result := router.Match(ctx, example)
			if result.Tool != name {
				problems = append(problems, fmt.Sprintf("%s: Example %q routed to %q (reason %s, top %v); each Example must route to its own Tool", name, example, result.Tool, result.Reason, result.Top))
			}
		}
	}
	return problems, nil
}

// semanticTuning is the shipped Fast path tuning the conformance suite uses, so
// it checks the same threshold and margin production does.
type semanticTuning struct {
	Model     string
	Threshold float64
	Margin    float64
	Joiners   []string
}

// loadShippedTuning reads assistant/config.yaml, found by walking up from the
// test package directory. It reads only the semantic settings, so unrelated
// config validation cannot make a module conformance test fail.
func loadShippedTuning() (semanticTuning, error) {
	path, err := findAssistantConfig()
	if err != nil {
		return semanticTuning{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticTuning{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg struct {
		Embed struct {
			Model string `yaml:"model"`
		} `yaml:"embed"`
		Semantic struct {
			Threshold float64  `yaml:"threshold"`
			Margin    float64  `yaml:"margin"`
			Joiners   []string `yaml:"joiners"`
		} `yaml:"semantic"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return semanticTuning{}, fmt.Errorf("parse %s: %w", path, err)
	}
	tuning := semanticTuning{
		Model:     strings.TrimSpace(cfg.Embed.Model),
		Threshold: cfg.Semantic.Threshold,
		Margin:    cfg.Semantic.Margin,
		Joiners:   cfg.Semantic.Joiners,
	}
	if tuning.Model == "" {
		return semanticTuning{}, errors.New("config.yaml has no embed.model")
	}
	if tuning.Threshold <= 0 || tuning.Threshold > 1 {
		return semanticTuning{}, fmt.Errorf("config.yaml semantic.threshold %.3f is outside (0, 1]", tuning.Threshold)
	}
	if tuning.Margin < 0 || tuning.Margin >= 1 {
		return semanticTuning{}, fmt.Errorf("config.yaml semantic.margin %.3f is outside [0, 1)", tuning.Margin)
	}
	return tuning, nil
}

// findAssistantConfig walks up from the package's working directory to the
// assistant module root (go.mod and config.yaml side by side).
func findAssistantConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			configPath := filepath.Join(dir, "config.yaml")
			if _, err := os.Stat(configPath); err == nil {
				return configPath, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("assistant/config.yaml not found above the test working directory")
		}
		dir = parent
	}
}

// testDeps gives modules harmless seams: no audio device, no speech, a fixed
// clock and a known place. Registration never calls a handler, so nothing
// outside the test process is touched.
func testDeps() tool.Deps {
	at := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	return tool.Deps{
		Clock:  func() time.Time { return at },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
		},
		Speaker: discardSpeaker{},
		Audio:   discardAudio{},
		Secrets: discardSecrets{},
	}
}

type discardSecrets struct{}

func (discardSecrets) Get(string) (string, bool) { return "", false }

type discardSpeaker struct{}

func (discardSpeaker) SayText(context.Context, string) error { return nil }

type discardAudio struct{}

func (discardAudio) SetVolume(context.Context, int) error    { return nil }
func (discardAudio) AdjustVolume(context.Context, int) error { return nil }
func (discardAudio) Stop(context.Context) error              { return nil }

// nameSet snapshots the registry before one module registers.
func nameSet(registry *tool.Registry) map[string]bool {
	names := registry.Names()
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// addedNames returns the names a module added, in sorted order.
func addedNames(registry *tool.Registry, before map[string]bool) []string {
	var added []string
	for _, name := range registry.Names() {
		if !before[name] {
			added = append(added, name)
		}
	}
	return added
}

func sortedKeys(samples map[string]any) []string {
	keys := make([]string, 0, len(samples))
	for key := range samples {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
