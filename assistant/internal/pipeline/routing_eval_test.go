package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/embed"
	"github.com/Jairik/clanka-bot/assistant/internal/focus"
	"github.com/Jairik/clanka-bot/assistant/internal/location"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/audiocontrol"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/timer"
	"github.com/Jairik/clanka-bot/assistant/tools/weather"
	"gopkg.in/yaml.v3"
)

// routingEvalTools is the complete Fast path surface this corpus must cover.
var routingEvalTools = []string{
	clock.Name,
	clock.DateName,
	weather.Name,
	audiocontrol.VolumeName,
	audiocontrol.StopName,
	timer.SetName,
	timer.CancelName,
	timer.RemainingName,
	timer.ListName,
}

const (
	routingEvalPositivesPerTool = 20
	routingEvalMinNegatives     = 50
	// routingEvalPrecisionFloor is the user-confirmed minimum precision for
	// each Fast path Tool (#26). Overall precision never excuses a weak Tool.
	routingEvalPrecisionFloor = 0.95
)

// routingFixture is the checked-in oracle. Positives are keyed by the Tool the
// utterance must reach; negatives must not reach the Fast path at all.
type routingFixture struct {
	Positives map[string][]string `yaml:"positives"`
	Negatives []string            `yaml:"negatives"`
	Compounds []string            `yaml:"compounds"`
	Focus     []routingFocusCase  `yaml:"focus"`
}

// routingFocusCase is one shared-phrase case: the holders present, the intent
// the utterance must deliver, and which holder must take it. An empty Intent
// means no holder takes it and the utterance must fall through.
type routingFocusCase struct {
	Utterance string   `yaml:"utterance"`
	Holders   []string `yaml:"holders"`
	Intent    string   `yaml:"intent"`
	HandledBy string   `yaml:"handled_by"`
}

// routingConfig is the subset of assistant/config.yaml the evaluation reads, so
// the test exercises the real Control phrase table and the shipped tuning.
type routingConfig struct {
	ControlPhrases map[string]string `yaml:"control_phrases"`
	Semantic       struct {
		Threshold float64  `yaml:"threshold"`
		Margin    float64  `yaml:"margin"`
		Joiners   []string `yaml:"joiners"`
	} `yaml:"semantic"`
}

// routingBaseline is the checked-in Map 1 accuracy the evaluation must not drop
// below. Counts, not rounded floats, so the gate is exact.
type routingBaseline struct {
	TP                int `yaml:"tp"`
	FP                int `yaml:"fp"`
	Positives         int `yaml:"positives"`
	NegativeMisroutes int `yaml:"negative_misroutes"`
	CompoundMisroutes int `yaml:"compound_misroutes"`
}

func (b routingBaseline) precision() float64 {
	if b.TP+b.FP == 0 {
		return 0
	}
	return float64(b.TP) / float64(b.TP+b.FP)
}

func (b routingBaseline) recall() float64 {
	if b.Positives == 0 {
		return 0
	}
	return float64(b.TP) / float64(b.Positives)
}

func baselinePath() string {
	return filepath.Join("testdata", "routing_baseline.yaml")
}

func loadRoutingBaseline(t *testing.T) routingBaseline {
	t.Helper()
	raw, err := os.ReadFile(baselinePath())
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var baseline routingBaseline
	if err := yaml.Unmarshal(raw, &baseline); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	return baseline
}

func fixturePath() string {
	return filepath.Join("testdata", "routing_eval.yaml")
}

func loadRoutingFixture(t *testing.T) routingFixture {
	t.Helper()
	raw, err := os.ReadFile(fixturePath())
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture routingFixture
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return fixture
}

func loadRoutingConfig(t *testing.T) routingConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	var cfg routingConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}
	return cfg
}

// TestRoutingEvalFixtureShape keeps the corpus honest without the embed
// service: every Fast path Tool is covered and the negatives are not thinned
// out to make precision easy.
func TestRoutingEvalFixtureShape(t *testing.T) {
	fixture := loadRoutingFixture(t)

	seen := make(map[string]bool, len(fixture.Positives))
	for name, utterances := range fixture.Positives {
		seen[name] = true
		if len(utterances) < routingEvalPositivesPerTool {
			t.Errorf("%s has %d positives, want at least %d", name, len(utterances), routingEvalPositivesPerTool)
		}
		for i, utterance := range utterances {
			if strings.TrimSpace(utterance) == "" {
				t.Errorf("%s positive %d is empty", name, i)
			}
		}
	}
	for _, name := range routingEvalTools {
		if !seen[name] {
			t.Errorf("fixture is missing Fast path Tool %q", name)
		}
	}
	for name := range fixture.Positives {
		if !contains(routingEvalTools, name) {
			t.Errorf("fixture labels unknown Fast path Tool %q", name)
		}
	}
	if total := len(fixture.Negatives) + len(fixture.Compounds); total < routingEvalMinNegatives {
		t.Errorf("fixture has %d negatives (including compounds), want at least %d", total, routingEvalMinNegatives)
	}
	for i, utterance := range fixture.Negatives {
		if strings.TrimSpace(utterance) == "" {
			t.Errorf("negative %d is empty", i)
		}
	}
	if len(fixture.Compounds) == 0 {
		t.Error("fixture has no compound negatives")
	}
	for i, utterance := range fixture.Compounds {
		if strings.TrimSpace(utterance) == "" {
			t.Errorf("compound %d is empty", i)
		}
	}
}

// TestRoutingEvalBaselineShape keeps the recorded Map 1 baseline honest
// without the embed service: it must describe a measurable, non-negative
// result.
func TestRoutingEvalBaselineShape(t *testing.T) {
	baseline := loadRoutingBaseline(t)
	if baseline.Positives <= 0 {
		t.Errorf("baseline positives = %d, want a positive count", baseline.Positives)
	}
	if baseline.TP < 0 || baseline.FP < 0 {
		t.Errorf("baseline tp = %d, fp = %d; counts must not be negative", baseline.TP, baseline.FP)
	}
	if baseline.TP+baseline.FP == 0 {
		t.Error("baseline has no predictions; precision would not be measurable")
	}
	if baseline.TP > baseline.Positives {
		t.Errorf("baseline tp = %d, positives = %d; TP cannot exceed positives", baseline.TP, baseline.Positives)
	}
	if baseline.NegativeMisroutes < 0 || baseline.CompoundMisroutes < 0 {
		t.Errorf("baseline misroutes = %d negatives, %d compounds; counts must not be negative",
			baseline.NegativeMisroutes, baseline.CompoundMisroutes)
	}
}

// evalAudio records audio Tool calls without touching a real sink.
type evalAudio struct{}

func (*evalAudio) SetVolume(context.Context, int) error    { return nil }
func (*evalAudio) AdjustVolume(context.Context, int) error { return nil }
func (*evalAudio) Stop(context.Context) error              { return nil }

// evalRegistry wires the real Tools with test-safe dependencies. Weather gets
// a local upstream so every weather positive executes the handler, but no
// request leaves the machine.
func evalRegistry(t *testing.T) (*tool.Registry, *timer.Manager) {
	t.Helper()
	reg := tool.NewRegistry(5 * time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)

	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	if err := clock.RegisterDate(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register date: %v", err)
	}

	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"Berlin","latitude":52.52,"longitude":13.41,"timezone":"Europe/Berlin"}]}`)
	}))
	t.Cleanup(geo.Close)
	forecast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"latitude":52.52,"longitude":13.41,"timezone":"Europe/Berlin",
			"current":{"time":"2026-11-11T16:00","interval":900,"temperature_2m":8.4,
			"apparent_temperature":6.1,"relative_humidity_2m":70,"weather_code":2,"wind_speed_10m":9.2}}`)
	}))
	t.Cleanup(forecast.Close)

	if err := weather.Register(reg, weather.Deps{
		GeocodingURL: geo.URL,
		ForecastURL:  forecast.URL,
		Location: func() (location.Location, bool) {
			return location.Location{City: "Berlin", Region: "Berlin", Timezone: "Europe/Berlin"}, true
		},
	}); err != nil {
		t.Fatalf("register weather: %v", err)
	}
	if err := audiocontrol.Register(reg, &evalAudio{}); err != nil {
		t.Fatalf("register audio: %v", err)
	}
	manager := timer.NewManager(func() time.Time { return fixed }, nil, nil)
	if err := timer.Register(reg, manager); err != nil {
		t.Fatalf("register timer: %v", err)
	}
	return reg, manager
}

type evalVerdict struct {
	Tool   string
	Path   tool.Path
	Reason string
	Top    []pipeline.ToolScore
}

type evalCounts struct {
	positives int
	tp        int
	// controlTP counts TP reached by an exact Control phrase, not the semantic router.
	controlTP int
	fp        int
	fn        int
}

func (c evalCounts) semanticTP() int { return c.tp - c.controlTP }

// precision is 0 with no predictions, so an empty denominator cannot clear the floor.
func (c evalCounts) precision() float64 {
	if c.tp+c.fp == 0 {
		return 0
	}
	return float64(c.tp) / float64(c.tp+c.fp)
}

func (c evalCounts) recall() float64 {
	if c.positives == 0 {
		return 0
	}
	return float64(c.tp) / float64(c.positives)
}

func envFloat(t *testing.T, key string, fallback float64) float64 {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s=%q is not a number: %v", key, raw, err)
	}
	return value
}

// TestRoutingEvalAgainstMiniLM runs every labelled utterance through the real
// pipeline, the real Tools and the real MiniLM embed service. It is skipped
// when ROUTING_EVAL_EMBED_URL is unset so the normal suite stays offline.
//
// Threshold and margin default to assistant/config.yaml; set
// ROUTING_EVAL_THRESHOLD / ROUTING_EVAL_MARGIN to sweep candidates. The 0.95
// per-Tool precision floor is fixed and cannot be overridden.
func TestRoutingEvalAgainstMiniLM(t *testing.T) {
	embedURL := strings.TrimSpace(os.Getenv("ROUTING_EVAL_EMBED_URL"))
	if embedURL == "" {
		t.Skip("set ROUTING_EVAL_EMBED_URL to the MiniLM embed service to run the routing evaluation")
	}

	fixture := loadRoutingFixture(t)
	cfg := loadRoutingConfig(t)
	baseline := loadRoutingBaseline(t)
	threshold := envFloat(t, "ROUTING_EVAL_THRESHOLD", cfg.Semantic.Threshold)
	margin := envFloat(t, "ROUTING_EVAL_MARGIN", cfg.Semantic.Margin)
	if threshold <= 0 || threshold > 1 {
		t.Fatalf("threshold %.3f is outside (0, 1]", threshold)
	}
	if margin < 0 || margin >= 1 {
		t.Fatalf("margin %.3f is outside [0, 1)", margin)
	}
	if len(cfg.ControlPhrases) == 0 {
		t.Fatal("config.yaml has no Control phrases; the evaluation would not exercise them")
	}
	if len(cfg.Semantic.Joiners) == 0 {
		t.Fatal("config.yaml has no compound joiners; the evaluation would not exercise compounds")
	}
	floor := routingEvalPrecisionFloor

	reg, manager := evalRegistry(t)
	router := pipeline.NewSemanticRouter(reg, embed.NewClient(embedURL, "sentence-transformers/all-MiniLM-L6-v2"), pipeline.SemanticConfig{
		Threshold: threshold,
		Margin:    margin,
		Joiners:   cfg.Semantic.Joiners,
		// Routing quality, not latency (#27), is under test. Allow a slow CPU
		// embed call rather than turn a timeout into a routing miss.
		Timeout: 10 * time.Second,
	})
	ctx := context.Background()
	if err := router.Build(ctx); err != nil {
		t.Fatalf("build semantic router against %s: %v", embedURL, err)
	}
	if got := router.ToolCount(); got != len(routingEvalTools) {
		t.Fatalf("semantic router indexed %d Tools, want %d", got, len(routingEvalTools))
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	runner := newFakeRunner()
	runner.result = agent.Result{Text: "agent answer"}
	var focusRegistry focus.Registry
	p, err := pipeline.New(reg, cfg.ControlPhrases, logger,
		pipeline.WithSemanticRouter(router),
		pipeline.WithAgent(runner, nil),
		// The corpus runs with no Focus holder, which is the production
		// fall-through case; the Focus cases claim holders per case.
		pipeline.WithFocus(&focusRegistry),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	classify := func(utterance string) evalVerdict {
		t.Helper()
		logBuf.Reset()
		reply := p.Handle(ctx, request(), utterance)
		// Timers must not outlive their utterance, and every call starts from
		// an empty manager so timer.list and timer.cancel are deterministic.
		manager.Cancel("")

		reason := ""
		var top []pipeline.ToolScore
		if line := strings.TrimSpace(logBuf.String()); line != "" {
			var record struct {
				Reason string               `json:"fallthrough_reason"`
				Top    []pipeline.ToolScore `json:"top_tools"`
			}
			if err := json.Unmarshal([]byte(line), &record); err == nil {
				reason = record.Reason
				top = record.Top
			}
		}
		return evalVerdict{Tool: reply.Tool, Path: reply.Path, Reason: reason, Top: top}
	}

	counts := make(map[string]*evalCounts, len(routingEvalTools))
	for _, name := range routingEvalTools {
		counts[name] = &evalCounts{}
	}

	for _, name := range routingEvalTools {
		for _, utterance := range fixture.Positives[name] {
			counts[name].positives++
			got := classify(utterance)
			if got.Tool == name && (got.Path == tool.PathFast || got.Path == tool.PathControlPhrase) {
				counts[name].tp++
				if got.Path == tool.PathControlPhrase {
					counts[name].controlTP++
				}
				continue
			}
			counts[name].fn++
			if got.Tool != "" {
				if other, ok := counts[got.Tool]; ok {
					other.fp++
				}
			}
			t.Logf("MISS positive tool=%s utterance=%q got_tool=%s path=%s reason=%s top=%v", name, utterance, got.Tool, got.Path, got.Reason, got.Top)
		}
	}

	negativeMisrouted := 0
	for _, utterance := range fixture.Negatives {
		got := classify(utterance)
		if got.Tool != "" && (got.Path == tool.PathFast || got.Path == tool.PathControlPhrase) {
			negativeMisrouted++
			if other, ok := counts[got.Tool]; ok {
				other.fp++
			}
			t.Logf("MISS negative utterance=%q reached tool=%s path=%s", utterance, got.Tool, got.Path)
		}
	}

	compoundMisrouted := 0
	for _, utterance := range fixture.Compounds {
		got := classify(utterance)
		if got.Tool != "" && (got.Path == tool.PathFast || got.Path == tool.PathControlPhrase) {
			compoundMisrouted++
			if other, ok := counts[got.Tool]; ok {
				other.fp++
			}
			t.Logf("MISS compound utterance=%q reached tool=%s path=%s", utterance, got.Tool, got.Path)
		}
	}

	// Focus cases use the same pipeline and phrase table; only the holders
	// differ. A delivered case must take the Focus path, and a fall-through
	// case must not.
	focusDelivered, focusFellThrough := runFocusEvalCases(t, p, &focusRegistry, fixture.Focus, classify)
	t.Logf("ROUTING_EVAL_FOCUS_TOTAL total=%d delivered=%d fallthrough=%d", len(fixture.Focus), focusDelivered, focusFellThrough)

	var total evalCounts
	t.Logf("ROUTING_EVAL threshold=%.3f margin=%.3f positives=%d negatives=%d compounds=%d floor=%.3f",
		threshold, margin, sumPositives(counts), len(fixture.Negatives), len(fixture.Compounds), floor)
	for _, name := range routingEvalTools {
		c := counts[name]
		total.positives += c.positives
		total.tp += c.tp
		total.controlTP += c.controlTP
		total.fp += c.fp
		total.fn += c.fn
		t.Logf("ROUTING_EVAL_TOOL %s positives=%d tp=%d control_tp=%d semantic_tp=%d fp=%d fn=%d precision=%.4f recall=%.4f",
			name, c.positives, c.tp, c.controlTP, c.semanticTP(), c.fp, c.fn, c.precision(), c.recall())
		// A Tool with no predictions has no measurable precision; fail and disclose it.
		if c.tp+c.fp == 0 {
			t.Errorf("%s made no Fast path predictions in %d positives, so its precision is not measurable and cannot clear the %.2f floor", name, c.positives, floor)
			continue
		}
		if c.semanticTP() == 0 {
			t.Errorf("%s cleared the floor only through Control phrases (%d/%d TP); the semantic router was never exercised for it", name, c.controlTP, c.tp)
		}
		if c.precision() < floor {
			t.Errorf("%s precision = %.3f (%d/%d), below floor %.3f", name, c.precision(), c.tp, c.tp+c.fp, floor)
		}
	}
	t.Logf("ROUTING_EVAL_NEGATIVES total=%d misrouted=%d", len(fixture.Negatives), negativeMisrouted)
	t.Logf("ROUTING_EVAL_COMPOUNDS total=%d misrouted=%d", len(fixture.Compounds), compoundMisrouted)
	t.Logf("ROUTING_EVAL_TOTAL tp=%d control_tp=%d semantic_tp=%d fp=%d fn=%d precision=%.4f recall=%.4f",
		total.tp, total.controlTP, total.semanticTP(), total.fp, total.fn, total.precision(), total.recall())
	t.Logf("ROUTING_EVAL_BASELINE tp=%d fp=%d positives=%d negative_misroutes=%d compound_misroutes=%d precision=%.4f recall=%.4f",
		baseline.TP, baseline.FP, baseline.Positives, baseline.NegativeMisroutes, baseline.CompoundMisroutes,
		baseline.precision(), baseline.recall())

	// CI gates on the recorded Map 1 baseline. New positives may raise recall,
	// but no change may lower it or the precision/misroute counts.
	if total.precision() < baseline.precision() {
		t.Errorf("overall precision = %.4f (%d/%d), below the Map 1 baseline %.4f (%d/%d)",
			total.precision(), total.tp, total.tp+total.fp, baseline.precision(), baseline.TP, baseline.TP+baseline.FP)
	}
	if total.recall() < baseline.recall() {
		t.Errorf("overall recall = %.4f (%d/%d), below the Map 1 baseline %.4f (%d/%d)",
			total.recall(), total.tp, total.positives, baseline.recall(), baseline.TP, baseline.Positives)
	}
	if negativeMisrouted > baseline.NegativeMisroutes {
		t.Errorf("negatives misrouted = %d, above the Map 1 baseline %d", negativeMisrouted, baseline.NegativeMisroutes)
	}
	if compoundMisrouted > baseline.CompoundMisroutes {
		t.Errorf("compounds misrouted = %d, above the Map 1 baseline %d", compoundMisrouted, baseline.CompoundMisroutes)
	}
}

func sumPositives(counts map[string]*evalCounts) int {
	total := 0
	for _, c := range counts {
		total += c.positives
	}
	return total
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
