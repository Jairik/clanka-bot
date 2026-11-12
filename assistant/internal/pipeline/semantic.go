package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/phrase"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
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

// SemanticRouter holds Tool example vectors computed once at Build.
type SemanticRouter struct {
	registry *tool.Registry
	embedder Embedder
	cfg      SemanticConfig

	examples []example
	dims     int
	tools    int
	ready    bool
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

// Build embeds every eligible Tool example once. Call it before requests.
func (r *SemanticRouter) Build(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.examples = nil
	r.tools = 0
	r.dims = 0
	r.ready = false
	if r.registry == nil {
		return fmt.Errorf("semantic router: registry is required")
	}
	if r.embedder == nil {
		return fmt.Errorf("semantic router: embedder is required")
	}

	type ref struct{ tool, text string }
	var refs []ref
	for _, name := range r.registry.Names() {
		info, ok := r.registry.Info(name)
		if !ok || !FastPathEligible(info) {
			continue
		}
		for _, text := range info.Examples {
			if strings.TrimSpace(text) == "" {
				continue
			}
			refs = append(refs, ref{tool: name, text: text})
		}
	}
	if len(refs) == 0 {
		r.ready = true
		return nil
	}

	texts := make([]string, len(refs))
	for i, ref := range refs {
		texts[i] = ref.text
	}
	vectors, err := r.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("semantic router: embed examples: %w", err)
	}
	if len(vectors) != len(texts) {
		return fmt.Errorf("semantic router: got %d example vectors for %d examples", len(vectors), len(texts))
	}
	seen := make(map[string]bool, len(refs))
	for i, vec := range vectors {
		if len(vec) == 0 {
			return fmt.Errorf("semantic router: example vector %d is empty", i)
		}
		if r.dims == 0 {
			r.dims = len(vec)
		} else if len(vec) != r.dims {
			return fmt.Errorf("semantic router: example vector %d has %d dimensions, want %d", i, len(vec), r.dims)
		}
		r.examples = append(r.examples, example{tool: refs[i].tool, vec: normalize(vec)})
		seen[refs[i].tool] = true
	}
	r.tools = len(seen)
	r.ready = true
	return nil
}

// ToolCount returns how many eligible Tools are indexed.
func (r *SemanticRouter) ToolCount() int {
	if r == nil {
		return 0
	}
	return r.tools
}

// Match scores a transcript against the indexed examples and applies the
// threshold, margin, compound and eligibility rules.
func (r *SemanticRouter) Match(ctx context.Context, transcript string) SemanticResult {
	result := SemanticResult{Extraction: "none", Top: []ToolScore{}}
	if r == nil || !r.ready {
		result.Reason = reasonSemanticDisabled
		return result
	}
	if len(r.examples) == 0 {
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
	if len(vectors) != 1 || len(vectors[0]) != r.dims {
		result.Reason = reasonEmbedFailed
		result.Err = fmt.Errorf("semantic router: utterance vector shape %d/%d, want 1/%d", len(vectors), vectorLen(vectors), r.dims)
		return result
	}
	query := normalize(vectors[0])

	best := make(map[string]float64, r.tools)
	for _, ex := range r.examples {
		score := dot(query, ex.vec)
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
// to this Tool: read/act Permission, a Speak renderer and either Extract or no
// required input fields. The conformance suite uses the same rule.
func FastPathEligible(info tool.Info) bool {
	if len(info.Examples) == 0 || !info.HasSpeak {
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

func normalize(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return append([]float32{}, vec...)
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = float32(float64(v) / norm)
	}
	return out
}

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func vectorLen(vectors [][]float32) int {
	if len(vectors) == 0 {
		return 0
	}
	return len(vectors[0])
}
