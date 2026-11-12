package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/vector"
)

// Fall-through reasons reported in the request log.
const (
	reasonSemanticDisabled = "semantic_disabled"
	reasonSemanticNoTools  = "no_semantic_tools"
	reasonSemanticNoMatch  = "no_match"
	reasonEmbedFailed      = "embed_failed"
	reasonBelowThreshold   = "below_threshold"
	reasonBelowMargin      = "below_margin"
	reasonCompound         = "compound"
	reasonExtractFailed    = "extract_failed"
)

// Embedder is the semantic router's only outside dependency.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ToolScore is one Tool's best-example cosine for a transcript.
type ToolScore struct {
	Tool  string  `json:"tool"`
	Score float64 `json:"score"`
}

// SemanticConfig tunes nearest-example matching.
type SemanticConfig struct {
	Threshold float64
	Margin    float64
	Joiners   []string
	Timeout   time.Duration
}

// SemanticRouter holds Tool example vectors. Build fills the live set; a later
// rebuild builds a new set off to the side and Swap installs it, so matching
// keeps using the old vectors until the replacements are ready.
type SemanticRouter struct {
	registry *tool.Registry
	embedder Embedder
	cfg      SemanticConfig

	// source, when set, supplies the effective Examples for each Tool. A nil
	// source uses the code declarations.
	source ExamplesSource

	// live is the current example set. Readers load it once per Match, so a
	// concurrent Swap can never mix vectors from two builds.
	live atomic.Pointer[ExampleSet]
}

// ExamplesSource returns the effective Examples per Tool. A missing Tool falls
// back to its code declaration only when the whole map is nil; an explicit
// empty list removes that Tool from the index.
type ExamplesSource interface {
	Examples() map[string][]string
}

// ExampleSet is one built, immutable example index. It is created by BuildSet
// and installed by Swap.
type ExampleSet struct {
	examples []example
	dims     int
	tools    int
}

// ToolCount returns how many Tools the set indexes.
func (s *ExampleSet) ToolCount() int {
	if s == nil {
		return 0
	}
	return s.tools
}

type example struct {
	tool string
	vec  []float32
}

// SemanticResult is the router's decision for one transcript.
type SemanticResult struct {
	Tool       string
	Args       json.RawMessage
	Top        []ToolScore
	Margin     float64
	Extraction string // none, ok, not_ok
	Reason     string // empty when the utterance matched a Tool
	Err        error  // internal detail, logged but never spoken
}

// NewSemanticRouter builds an empty router; call Build before serving.
func NewSemanticRouter(reg *tool.Registry, embedder Embedder, cfg SemanticConfig) *SemanticRouter {
	return &SemanticRouter{registry: reg, embedder: embedder, cfg: cfg}
}

// SetExamplesSource supplies the effective Examples for Build and later
// rebuilds. Set it before Build.
func (r *SemanticRouter) SetExamplesSource(source ExamplesSource) {
	if r == nil {
		return
	}
	r.source = source
}

// Build embeds every eligible Tool example and swaps the result in. A failed
// build leaves the current vectors, if any, untouched.
func (r *SemanticRouter) Build(ctx context.Context) error {
	if r == nil {
		return nil
	}
	examples := map[string][]string(nil)
	if r.source != nil {
		examples = r.source.Examples()
	}
	set, err := r.BuildSet(ctx, examples)
	if err != nil {
		return err
	}
	r.Swap(set)
	return nil
}

// BuildSet embeds one explicit Examples snapshot without touching the live
// vectors. A nil map uses the code declarations; a non-nil map is used as-is,
// so an empty list removes that Tool from the index.
func (r *SemanticRouter) BuildSet(ctx context.Context, examples map[string][]string) (*ExampleSet, error) {
	if r == nil {
		return nil, fmt.Errorf("semantic router: router is required")
	}
	if r.registry == nil {
		return nil, fmt.Errorf("semantic router: registry is required")
	}
	if r.embedder == nil {
		return nil, fmt.Errorf("semantic router: embedder is required")
	}

	type ref struct{ tool, text string }
	var refs []ref
	for _, name := range r.registry.Names() {
		info, ok := r.registry.Info(name)
		if !ok {
			continue
		}
		list := info.Examples
		if examples != nil {
			list = examples[name]
		}
		if !fastPathEligible(info, list) {
			continue
		}
		for _, text := range list {
			if strings.TrimSpace(text) == "" {
				continue
			}
			refs = append(refs, ref{tool: name, text: text})
		}
	}
	set := &ExampleSet{}
	if len(refs) == 0 {
		return set, nil
	}

	texts := make([]string, len(refs))
	for i, ref := range refs {
		texts[i] = ref.text
	}
	vectors, err := r.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("semantic router: embed examples: %w", err)
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("semantic router: got %d example vectors for %d examples", len(vectors), len(texts))
	}
	seen := make(map[string]bool, len(refs))
	for i, vec := range vectors {
		if len(vec) == 0 {
			return nil, fmt.Errorf("semantic router: example vector %d is empty", i)
		}
		if set.dims == 0 {
			set.dims = len(vec)
		} else if len(vec) != set.dims {
			return nil, fmt.Errorf("semantic router: example vector %d has %d dimensions, want %d", i, len(vec), set.dims)
		}
		set.examples = append(set.examples, example{tool: refs[i].tool, vec: vector.Normalize(vec)})
		seen[refs[i].tool] = true
	}
	set.tools = len(seen)
	return set, nil
}

// Swap installs a built set. Readers see either the complete old set or the
// complete new one, never a partial rebuild.
func (r *SemanticRouter) Swap(set *ExampleSet) {
	if r == nil || set == nil {
		return
	}
	r.live.Store(set)
}

// ToolCount returns how many eligible Tools are indexed.
func (r *SemanticRouter) ToolCount() int {
	if r == nil {
		return 0
	}
	return r.live.Load().ToolCount()
}

// Match scores a transcript against the indexed examples and applies the
// threshold, margin, compound and eligibility rules.
func (r *SemanticRouter) Match(ctx context.Context, transcript string) SemanticResult {
	result := SemanticResult{Extraction: "none", Top: []ToolScore{}}
	if r == nil {
		result.Reason = reasonSemanticDisabled
		return result
	}
	set := r.live.Load()
	if set == nil {
		result.Reason = reasonSemanticDisabled
		return result
	}
	if len(set.examples) == 0 {
		result.Reason = reasonSemanticNoTools
		return result
	}

	if r.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
	}
	// STT adds a full stop (and sometimes capitalises) to every transcript.
	// Embed the same words the examples use: trailing sentence punctuation and
	// extra whitespace must not push a timer request under the margin (#34).
	// Case is preserved so names keep their signal.
	vectors, err := r.embedder.Embed(ctx, []string{phrase.TrimSentence(transcript)})
	if err != nil {
		result.Reason = reasonEmbedFailed
		result.Err = err
		return result
	}
	if len(vectors) != 1 || len(vectors[0]) != set.dims {
		result.Reason = reasonEmbedFailed
		result.Err = fmt.Errorf("semantic router: utterance vector shape %d/%d, want 1/%d", len(vectors), vectorLen(vectors), set.dims)
		return result
	}
	query := vector.Normalize(vectors[0])

	best := make(map[string]float64, set.tools)
	for _, ex := range set.examples {
		score := vector.Dot(query, ex.vec)
		if current, ok := best[ex.tool]; !ok || score > current {
			best[ex.tool] = score
		}
	}
	scores := make([]ToolScore, 0, len(best))
	for name, score := range best {
		scores = append(scores, ToolScore{Tool: name, Score: score})
	}
	// Every indexed Tool has at least one example, so scores cannot be empty.
	if len(scores) == 0 {
		result.Reason = reasonSemanticNoTools
		return result
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return scores[i].Tool < scores[j].Tool
	})
	result.Top = topN(scores, 3)
	result.Margin = scores[0].Score
	if len(scores) > 1 {
		result.Margin = scores[0].Score - scores[1].Score
	}
	if scores[0].Score < r.cfg.Threshold {
		result.Reason = reasonBelowThreshold
		return result
	}
	if len(scores) > 1 && scores[1].Score >= r.cfg.Threshold && hasJoiner(transcript, r.cfg.Joiners) {
		result.Reason = reasonCompound
		return result
	}
	if result.Margin < r.cfg.Margin {
		result.Reason = reasonBelowMargin
		return result
	}

	args, extraction, ok := r.resolveArgs(scores[0].Tool, transcript)
	result.Extraction = extraction
	if !ok {
		result.Reason = reasonExtractFailed
		return result
	}
	result.Tool = scores[0].Tool
	result.Args = args
	return result
}

// resolveArgs applies Extract, or passes no arguments for no-argument Tools.
func (r *SemanticRouter) resolveArgs(name, transcript string) (json.RawMessage, string, bool) {
	info, ok := r.registry.Info(name)
	if !ok || !FastPathEligible(info) {
		return nil, "none", false
	}
	if !info.HasExtract {
		return nil, "none", true
	}
	args, ok := r.registry.Extract(name, transcript)
	if !ok {
		return nil, "not_ok", false
	}
	return args, "ok", true
}

// FastPathEligible reports whether the semantic router may send an utterance
// to this Tool: enabled, read/act Permission, a Speak renderer, at least one
// Example and either Extract or no required input fields. The conformance suite
// uses the same rule.
func FastPathEligible(info tool.Info) bool {
	return fastPathEligible(info, info.Examples)
}

// CanUseFastPath is FastPathEligible without the current Examples and enabled
// state. It answers whether a Tool could ever be routed by meaning, so the
// wording settings know which Tools can carry overridable Examples.
func CanUseFastPath(info tool.Info) bool {
	if !info.HasSpeak {
		return false
	}
	if info.Permission != tool.PermissionRead && info.Permission != tool.PermissionAct {
		return false
	}
	if info.HasExtract {
		return true
	}
	return !hasInputFields(info.InputSchema)
}

// fastPathEligible is the shared rule with one explicit Examples list, so a
// rebuilt index applies the same eligibility to effective overrides.
func fastPathEligible(info tool.Info, examples []string) bool {
	return info.Enabled && len(examples) > 0 && CanUseFastPath(info)
}

// hasInputFields reports whether a Tool's In struct declares any fields.
func hasInputFields(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return true // Unknown schema: stay conservative and keep it off the Fast path.
	}
	return len(s.Properties) > 0
}

var wordRE = regexp.MustCompile(`[a-z']+`)

// hasJoiner looks for a configured clause joiner as a whole word.
func hasJoiner(text string, joiners []string) bool {
	if len(joiners) == 0 {
		return false
	}
	set := make(map[string]bool, len(joiners))
	for _, j := range joiners {
		set[strings.ToLower(strings.TrimSpace(j))] = true
	}
	for _, word := range wordRE.FindAllString(strings.ToLower(text), -1) {
		if set[word] {
			return true
		}
	}
	return false
}

func topN(scores []ToolScore, n int) []ToolScore {
	if len(scores) > n {
		scores = scores[:n]
	}
	return append([]ToolScore{}, scores...)
}

func vectorLen(vectors [][]float32) int {
	if len(vectors) == 0 {
		return 0
	}
	return len(vectors[0])
}
