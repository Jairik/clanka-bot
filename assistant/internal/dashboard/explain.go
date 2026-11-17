package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
)

// RoutingExplainer computes the side-effect-free routing decision for a transcript.
type RoutingExplainer interface {
	Explain(ctx context.Context, transcript string) pipeline.RoutingDecision
}

// routingScoreView is one tool's cosine similarity score in the explain response.
type routingScoreView struct {
	Tool  string  `json:"tool"`
	Score float64 `json:"score"`
}

// routingExplainView is the read-only explain routing view returned to the dashboard.
type routingExplainView struct {
	Path              string             `json:"path"`
	Tool              string             `json:"tool,omitempty"`
	Args              json.RawMessage    `json:"args,omitempty"`
	Top               []routingScoreView `json:"top"`
	Margin            float64            `json:"margin"`
	Extract           string             `json:"extract,omitempty"`
	FallthroughReason string             `json:"fallthrough_reason,omitempty"`
	ChainSplit        bool               `json:"chain_split"`
	FocusOwner        string             `json:"focus_owner,omitempty"`
	Decision          string             `json:"decision,omitempty"`
}

// routingExplain handles POST /api/ui/routing/explain to return the side-effect-free routing decision.
func (h *Handler) routingExplain(w http.ResponseWriter, r *http.Request) {
	if h.explainer == nil {
		http.Error(w, "routing explainer is not configured", http.StatusServiceUnavailable)
		return
	}

	var text string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		text = in.Text
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		text = r.FormValue("text")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	decision := h.explainer.Explain(r.Context(), text)
	scores := make([]routingScoreView, 0, len(decision.Top))
	for _, s := range decision.Top {
		scores = append(scores, routingScoreView{
			Tool:  s.Tool,
			Score: s.Score,
		})
	}

	view := routingExplainView{
		Path:              string(decision.Path),
		Tool:              decision.Tool,
		Args:              decision.Args,
		Top:               scores,
		Margin:            decision.Margin,
		Extract:           decision.Extraction,
		FallthroughReason: decision.Reason,
		ChainSplit:        decision.ChainSplit,
		FocusOwner:        decision.FocusOwner,
		Decision:          decision.Decision,
	}

	h.writeJSON(w, http.StatusOK, view)
}
