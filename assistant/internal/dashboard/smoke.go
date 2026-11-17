package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const (
	smokeTranscript = "what time is it"
	smokeTimeout    = 5 * time.Second
)

// smokeStagesResponse reports the measured pipeline stage timings for the smoke turn.
type smokeStagesResponse struct {
	RoutingMS int64 `json:"routing_ms"`
	ToolMS    int64 `json:"tool_ms"`
}

// smokeResponse is the contract returned by POST /api/ui/smoke.
type smokeResponse struct {
	Route  string              `json:"route"`
	Tool   string              `json:"tool"`
	Reply  string              `json:"reply"`
	Stages smokeStagesResponse `json:"stages"`
}

// smokeAPI executes one post-deploy smoke turn through the shared pipeline.
// It verifies that routing and Tool execution answer without speaking or
// mutating user-visible state.
func (h *Handler) smokeAPI(w http.ResponseWriter, r *http.Request) {
	if h.typed == nil {
		h.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "typed responder is not wired",
		})
		return
	}

	// One request at a time. A closed connection abandons the wait.
	select {
	case h.askLock <- struct{}{}:
		defer func() { <-h.askLock }()
	case <-r.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), smokeTimeout)
	defer cancel()

	requestID := tool.NewRequestID()
	// No Activity observer is attached: smoke turns are automated health probes
	// and must never appear in user activity.
	timeline := latency.New(requestID, latency.SourceSmoke)
	requestCtx := latency.WithContext(ctx, timeline)

	reply, err := h.typed.Ask(requestCtx, tool.Request{
		ID:     requestID,
		Path:   tool.PathFallthrough,
		Device: tool.DeviceSatellite,
	}, smokeTranscript)

	// Complete the timeline if the typed responder didn't do so already.
	if !timeline.Done() {
		outcome := latency.OutcomeNoAudio
		if requestCtx.Err() != nil {
			outcome = latency.OutcomeCancelled
		} else if err != nil || timeline.Failed() {
			outcome = latency.OutcomeFailed
		}
		latency.Complete(requestCtx, h.logger, outcome, err)
	}

	if requestCtx.Err() != nil {
		return
	}
	if err != nil {
		h.logger.Error("dashboard smoke request failed", "request_id", requestID, "error", err.Error())
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}

	var routingMS, toolMS int64
	if d, ok := timeline.Duration(latency.StageRouting); ok {
		routingMS = d.Milliseconds()
	}
	if d, ok := timeline.Duration(latency.StageToolExecute); ok {
		toolMS = d.Milliseconds()
	}

	h.writeJSON(w, http.StatusOK, smokeResponse{
		Route: string(reply.Path),
		Tool:  reply.Tool,
		Reply: reply.Text,
		Stages: smokeStagesResponse{
			RoutingMS: routingMS,
			ToolMS:    toolMS,
		},
	})
}
