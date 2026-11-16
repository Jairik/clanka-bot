package latency

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Budget is a Response latency target from #12, in milliseconds.
type Budget struct {
	Label string
	P50   float64
	P95   float64
}

// Response budgets by request path. Control phrases and Confirmation answers
// are Fast path requests; the Agent path gets its own, wider budget.
var (
	BudgetFast  = Budget{Label: "Fast path", P50: 1200, P95: 2000}
	BudgetAgent = Budget{Label: "Agent path", P50: 2500, P95: 4000}
)

// StageBudget is the per-stage latency budget from #12, in milliseconds. These
// numbers are the same expectations the dev-box run in docs/latency-dev-box.md
// was measured against; see the "Stage budgets" table in docs/latency.md. They
// let the report catch a regression that moves time from one stage into another
// while the overall Response stays within budget. A stage absent from this map
// (for example tool_execute, which is network-bound and conditional on the
// Tool's own latency) has no budget and is never flagged.
var StageBudget = map[Stage]Budget{
	StageEndpointing:   {Label: "endpointing", P50: 550, P95: 600},
	StageSTTFinalize:   {Label: "stt_finalize", P50: 200, P95: 250},
	StageRouting:       {Label: "routing", P50: 50, P95: 100},
	StageTTSFirstAudio: {Label: "tts_first_audio", P50: 150, P95: 200},
}

// AgentFirstTokenP50 is the #12 trigger for questioning the model choice:
// above it, flag the model first, before anything else.
const AgentFirstTokenP50 = 1200.0

// BudgetFor returns the Response latency budget for a routed path.
func BudgetFor(path string) (Budget, bool) {
	switch path {
	case "fast_path", "control_phrase", "confirmation", "focus_intent":
		return BudgetFast, true
	case "agent_path":
		return BudgetAgent, true
	default:
		return Budget{}, false
	}
}

// Record is one `request` log line as the assistant writes it.
type Record struct {
	Time        time.Time        `json:"time"`
	Msg         string           `json:"msg"`
	RequestID   string           `json:"request_id"`
	Source      string           `json:"source"`
	Path        string           `json:"path"`
	Outcome     string           `json:"outcome"`
	ResponseMS  *float64         `json:"response_ms"`
	Stages      map[string]int64 `json:"stages"`
	Checkpoints map[string]int64 `json:"checkpoints"`
}

// ReadRecords parses assistant JSON log lines, keeping request lines for the
// requested source. Lines from other services, or non-JSON output, are
// skipped so `docker compose logs` can be piped in directly.
func ReadRecords(r io.Reader, source string) ([]Record, error) {
	var records []Record
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Compose prefixes container output unless --no-log-prefix is used.
		if i := strings.IndexByte(line, '{'); i > 0 {
			line = line[i:]
		}
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Msg != "request" {
			continue
		}
		if source != "" && record.Source != source {
			continue
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

// Stat is a percentile summary in milliseconds.
type Stat struct {
	N   int
	P50 float64
	P95 float64
}

// PathSummary is one routed path's outcomes and timings.
type PathSummary struct {
	Path          string
	Attempts      int
	Outcomes      map[string]int
	Response      Stat
	Stages        map[string]Stat
	FirstChunk    Stat
	AgentToken    Stat
	ResponseCount int
}

// Summary is the whole report.
type Summary struct {
	Records int
	Paths   []PathSummary
}

// Summarize groups request records by path. Response latency and stage stats
// use only OutcomeOK requests; failed, cancelled and audio-less requests are
// counted in Outcomes but never treated as fast successes.
func Summarize(records []Record) Summary {
	grouped := make(map[string]*pathAccumulator)
	for _, record := range records {
		path := record.Path
		if path == "" {
			path = "(unrouted)"
		}
		acc := grouped[path]
		if acc == nil {
			acc = newPathAccumulator(path)
			grouped[path] = acc
		}
		acc.add(record)
	}

	paths := make([]string, 0, len(grouped))
	for path := range grouped {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	summary := Summary{Records: len(records)}
	for _, path := range paths {
		summary.Paths = append(summary.Paths, grouped[path].summary())
	}
	return summary
}

type pathAccumulator struct {
	path     string
	attempts int
	outcomes map[string]int
	response []float64
	stages   map[string][]float64
	chunk    []float64
	tokens   []float64
}

func newPathAccumulator(path string) *pathAccumulator {
	return &pathAccumulator{
		path:     path,
		outcomes: make(map[string]int),
		stages:   make(map[string][]float64),
	}
}

func (a *pathAccumulator) add(record Record) {
	a.attempts++
	outcome := record.Outcome
	if outcome == "" {
		outcome = "(missing)"
	}
	a.outcomes[outcome]++

	// A success is reply audio actually starting. Everything else is counted
	// separately and contributes no timing samples.
	if record.Outcome != string(OutcomeOK) {
		return
	}
	if record.ResponseMS != nil {
		a.response = append(a.response, *record.ResponseMS)
	}
	for name, value := range record.Stages {
		a.stages[name] = append(a.stages[name], float64(value))
	}
	if value, ok := record.Checkpoints[CheckpointFirstChunk+"_ms"]; ok {
		a.chunk = append(a.chunk, float64(value))
	}
	if value, ok := record.Stages[string(StageAgentFirstToken)+"_ms"]; ok {
		a.tokens = append(a.tokens, float64(value))
	}
}

func (a *pathAccumulator) summary() PathSummary {
	summary := PathSummary{
		Path:          a.path,
		Attempts:      a.attempts,
		Outcomes:      a.outcomes,
		Response:      statOf(a.response),
		Stages:        make(map[string]Stat, len(a.stages)),
		FirstChunk:    statOf(a.chunk),
		AgentToken:    statOf(a.tokens),
		ResponseCount: len(a.response),
	}
	for name, values := range a.stages {
		summary.Stages[name] = statOf(values)
	}
	return summary
}

func statOf(values []float64) Stat {
	if len(values) == 0 {
		return Stat{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return Stat{
		N:   len(sorted),
		P50: Percentile(sorted, 50),
		P95: Percentile(sorted, 95),
	}
}

// Percentile uses the nearest-rank definition on an ascending slice, which is
// the conservative choice for a small sample: p95 of 9 samples is the 9th.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Over reports whether a summary exceeds the path's budget. A zero Stat (no
// samples) never counts as over budget.
func (s PathSummary) Over(b Budget) (overP50, overP95 bool) {
	return s.ResponseCount > 0 && s.Response.P50 > b.P50, s.ResponseCount > 0 && s.Response.P95 > b.P95
}

// OverStage reports whether a named stage exceeds its per-stage budget. The
// stage name is resolved to the _ms log key, so callers pass the Stage value
// (for example "routing") rather than the raw log key. A stage that was not
// measured (absent from the summary, or zero samples) is not over budget and
// reports measured=false. The CLI owns no policy of its own: it only prints the
// flag this returns and never decides which stages are budgeted.
func (s PathSummary) OverStage(stage string, b Budget) (overP50, overP95, measured bool) {
	if !strings.HasSuffix(stage, "_ms") {
		stage += "_ms"
	}
	st, ok := s.Stages[stage]
	if !ok || st.N == 0 {
		return false, false, false
	}
	return st.P50 > b.P50, st.P95 > b.P95, true
}
