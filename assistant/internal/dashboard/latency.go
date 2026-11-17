package dashboard

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

// latencyDocumentation explains the sample filter for the dashboard panel.
const latencyDocumentation = "Sampled from successful requests with reply audio only; failed, cancelled and unmeasured requests are excluded."

// latencyStatView is a percentile summary in milliseconds.
type latencyStatView struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}

// latencyStageView is one measured stage's percentile timings.
type latencyStageView struct {
	Name  string  `json:"name"`
	Label string  `json:"label"`
	N     int     `json:"n"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
}

// latencyPathView is one routed path's latency summary.
type latencyPathView struct {
	Path          string                     `json:"path"`
	Label         string                     `json:"label"`
	Attempts      int                        `json:"attempts"`
	ResponseCount int                        `json:"response_count"`
	Outcomes      map[string]int             `json:"outcomes"`
	Response      latencyStatView            `json:"response"`
	Stages        map[string]latencyStatView `json:"stages"`
	StageList     []latencyStageView         `json:"stage_list"`
	FirstChunk    latencyStatView            `json:"first_chunk"`
	AgentToken    latencyStatView            `json:"agent_token"`
}

// latencyView is the latency panel's data model.
type latencyView struct {
	Window  string            `json:"window"`
	Records int               `json:"records"`
	Paths   []latencyPathView `json:"paths"`
	Notice  string            `json:"notice"`
}

// parseLatencyWindow parses the window query parameter, defaulting to 24 hours.
func parseLatencyWindow(raw string) (string, time.Duration) {
	switch strings.TrimSpace(raw) {
	case "7d":
		return "7d", 7 * 24 * time.Hour
	case "24h":
		return "24h", 24 * time.Hour
	default:
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return raw, d
		}
		return "24h", 24 * time.Hour
	}
}

// trimStageMS strips the _ms suffix from a stage name.
func trimStageMS(name string) string {
	if strings.HasSuffix(name, "_ms") {
		return strings.TrimSuffix(name, "_ms")
	}
	return name
}

// toLatencyStatView converts a latency.Stat to its view representation.
func toLatencyStatView(s latency.Stat) latencyStatView {
	return latencyStatView{
		N:   s.N,
		P50: s.P50,
		P95: s.P95,
	}
}

// buildLatencyView constructs the view model from the latency summary.
func (h *Handler) buildLatencyView(window string, summary latency.Summary) latencyView {
	view := latencyView{
		Window:  window,
		Records: summary.Records,
		Notice:  latencyDocumentation,
	}
	for _, ps := range summary.Paths {
		pv := latencyPathView{
			Path:          ps.Path,
			Label:         pathLabel(ps.Path),
			Attempts:      ps.Attempts,
			ResponseCount: ps.ResponseCount,
			Outcomes:      ps.Outcomes,
			Response:      toLatencyStatView(ps.Response),
			Stages:        make(map[string]latencyStatView, len(ps.Stages)),
			FirstChunk:    toLatencyStatView(ps.FirstChunk),
			AgentToken:    toLatencyStatView(ps.AgentToken),
		}
		for name, stat := range ps.Stages {
			trimmed := trimStageMS(name)
			pv.Stages[trimmed] = toLatencyStatView(stat)
		}

		// Build StageList in standard display order, only including measured stages (N > 0).
		for _, stage := range activity.StageOrder {
			name := string(stage)
			if stat, ok := pv.Stages[name]; ok && stat.N > 0 {
				pv.StageList = append(pv.StageList, latencyStageView{
					Name:  name,
					Label: stageLabel(name),
					N:     stat.N,
					P50:   stat.P50,
					P95:   stat.P95,
				})
			}
		}
		// Any custom stage not in activity.StageOrder
		var extra []string
		for name, stat := range pv.Stages {
			if stat.N > 0 && !isKnownLatencyStage(name) {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		for _, name := range extra {
			stat := pv.Stages[name]
			pv.StageList = append(pv.StageList, latencyStageView{
				Name:  name,
				Label: stageLabel(name),
				N:     stat.N,
				P50:   stat.P50,
				P95:   stat.P95,
			})
		}

		view.Paths = append(view.Paths, pv)
	}
	return view
}

// isKnownLatencyStage checks whether the stage name is in activity.StageOrder.
func isKnownLatencyStage(name string) bool {
	for _, s := range activity.StageOrder {
		if string(s) == name {
			return true
		}
	}
	return false
}

// latencyPage serves the latency percentiles view for the selected window.
func (h *Handler) latencyPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	windowParam, duration := parseLatencyWindow(r.URL.Query().Get("window"))
	var summary latency.Summary
	if h.activity != nil {
		var err error
		summary, err = h.activity.Latency(r.Context(), duration)
		if err != nil {
			h.logger.Warn("dashboard latency query", "error", err.Error())
		}
	}
	h.writeJSON(w, http.StatusOK, h.buildLatencyView(windowParam, summary))
}
