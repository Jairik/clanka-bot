package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const (
	// maxTypedFormBytes bounds the whole form read. It is larger than the text
	// limit so multipart/urlencoded overhead cannot reject a valid request.
	maxTypedFormBytes = 16 << 10
	// maxTypedTextRunes bounds one typed request. The voice path implicitly
	// bounds an utterance with the endpointer; the typed box needs its own.
	maxTypedTextRunes = 2000
	// activityDisplayLimit bounds one page. The store keeps more, so a refresh
	// still has history after a short burst.
	activityDisplayLimit = 50
)

// TypedResponder answers a typed Dashboard request through the normal pipeline.
// main passes a reply.Session built with no speaker, so the same routing,
// Confirmation, Permission, Local-only and history code runs without starting
// Pi audio. The implementation must complete the request timeline; reply.Session
// does that itself.
type TypedResponder interface {
	Ask(ctx context.Context, req tool.Request, transcript string) (pipeline.Reply, error)
}

// activityView is the Activity page's data model. Values are precomputed so the
// template holds no lookups.
type activityView struct {
	Owner          string
	Page           string
	Error          string
	Notice         string
	Draft          string
	HasActivity    bool
	HasTyped       bool
	DisplayLimit   int
	Just           *requestView
	Requests       []requestView
	JustRequestID  string
	PrivacyNotice  string
	LatencyNotice  string
	CollectionNote string
}

// requestView is one recent request row. Response latency is a pointer-like
// string plus a "measured" flag, because a typed request has no speech start
// and must never display an invented number.
type requestView struct {
	ID          string
	Time        string
	TimeISO     string
	Source      string
	SourceLabel string
	Path        string
	PathLabel   string
	Tool        string
	Outcome     string
	OutcomeText string
	OutcomePill string
	Response    string
	Measured    bool
	Error       string
	Transcript  string
	Reply       string
	LocalOnly   bool
	Pending     bool
	Stages      []timingView
	Checkpoints []timingView
	Calls       []callView
}

type timingView struct {
	Name  string
	Label string
	MS    string
}

type callView struct {
	Name      string
	LocalOnly bool
	Result    string
}

// activityPage renders the typed box and the recent request list.
func (h *Handler) activityPage(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	h.writeJSON(w, http.StatusOK, h.buildActivityView(r.URL.Query().Get("request"), "", "", ""))
}

// activityAsk runs one typed request through the shared pipeline. Overlapping
// submissions wait for the one slot; a disconnected waiter or runner is
// cancelled by its own request context, matching the voice turn's Barge-in
// semantics without cancelling the other request.
func (h *Handler) activityAsk(w http.ResponseWriter, r *http.Request) {
	if h.typed == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTypedFormBytes)
	if err := r.ParseForm(); err != nil {
		h.activityFail(w, r, http.StatusBadRequest, "Could not read the form.", "")
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	switch {
	case text == "":
		h.activityFail(w, r, http.StatusBadRequest, "Type a request first.", "")
		return
	case utf8.RuneCountInString(text) > maxTypedTextRunes:
		h.activityFail(w, r, http.StatusBadRequest,
			fmt.Sprintf("Keep the request to %d characters or fewer.", maxTypedTextRunes), text)
		return
	}

	h.runAsk(w, r, text, latency.SourceDashboard, nil)
}

// runAsk answers one Dashboard request (typed, or transcribed from the
// browser mic) through the shared pipeline and writes the refreshed Activity
// view. stages are timings measured before the request reached the pipeline,
// such as STT for a mic request.
func (h *Handler) runAsk(w http.ResponseWriter, r *http.Request, text string, source latency.Source, stages map[latency.Stage]time.Duration) {
	// One request at a time. Waiting on the request context means a
	// closed tab cannot leave an unreferenced request in the queue.
	select {
	case h.askLock <- struct{}{}:
		defer func() { <-h.askLock }()
	case <-r.Context().Done():
		return
	}

	requestID := tool.NewRequestID()
	var timelineOpts []latency.Option
	if h.activity != nil {
		timelineOpts = append(timelineOpts, latency.WithObserver(h.activity.Observe))
	}
	// A typed request has no endpointing, STT or reply audio, so its timeline
	// can never claim Response latency. Routing and Tool stages are still real.
	timeline := latency.New(requestID, source, timelineOpts...)
	for stage, d := range stages {
		timeline.Set(stage, d)
	}
	requestCtx := latency.WithContext(r.Context(), timeline)
	_, err := h.typed.Ask(requestCtx, tool.Request{
		ID:     requestID,
		Path:   tool.PathFallthrough,
		Device: tool.DeviceSatellite,
	}, text)
	// The real reply.Session completes the timeline itself. This fallback keeps
	// the one structured log line present for any other TypedResponder and
	// makes a test double unable to silently skip the Activity record.
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
		// The browser went away; nothing can display the answer.
		return
	}
	if err != nil {
		h.logger.Error("dashboard typed request",
			"request_id", requestID,
			"error", err.Error(),
		)
		h.activityFail(w, r, http.StatusInternalServerError, "That request could not be completed.", text)
		return
	}
	// The pipeline always returns text, including a helpful placeholder or an
	// Unavailable message; the view reads it straight from the Activity store.

	h.writeJSON(w, http.StatusOK, h.buildActivityView(requestID, "", "", ""))
}

// activityFail answers with the Activity view, the error and the draft to
// restore, under the real status code.
func (h *Handler) activityFail(w http.ResponseWriter, r *http.Request, status int, message, draft string) {
	view := h.buildActivityView("", message, "", draft)
	h.writeJSON(w, status, view)
}

// buildActivityView reads one bounded snapshot. justID highlights the reply
// that the POST just produced; it is only a lookup key, never user text.
func (h *Handler) buildActivityView(justID, errorText, notice, draft string) activityView {
	view := activityView{
		Owner:          h.owner,
		Page:           "activity",
		Error:          errorText,
		Draft:          draft,
		HasActivity:    h.activity != nil,
		HasTyped:       h.typed != nil,
		DisplayLimit:   activityDisplayLimit,
		JustRequestID:  justID,
		PrivacyNotice:  "Local-only Tool results appear only on this authenticated page. They are never sent to a model, an Agent prompt, a Notification, or any other egress path.",
		LatencyNotice:  "Response latency is shown only when the request actually produced reply audio. A typed request has no speech start or reply audio, so it shows stage timings and no invented Response latency.",
		CollectionNote: "One in-memory ring fed by the same completion that writes the structured request log. Nothing is persisted and nothing is read back from container logs.",
		Notice:         notice,
	}
	if h.activity == nil {
		return view
	}
	for _, entry := range h.activity.Recent(activityDisplayLimit) {
		request := h.requestView(entry)
		view.Requests = append(view.Requests, request)
		if entry.ID == justID {
			just := request
			view.Just = &just
		}
	}
	return view
}

func (h *Handler) requestView(entry activity.Entry) requestView {
	view := requestView{
		ID:          entry.ID,
		Time:        entry.At.Format("15:04:05"),
		TimeISO:     entry.At.Format(time.RFC3339),
		Source:      entry.Source,
		SourceLabel: activitySourceLabel(entry.Source),
		Path:        entry.Path,
		PathLabel:   pathLabel(entry.Path),
		Tool:        entry.Tool,
		Outcome:     entry.Outcome,
		OutcomeText: outcomeLabel(entry.Outcome),
		OutcomePill: outcomePill(entry.Outcome),
		Response:    "Not measured",
		Error:       entry.Error,
		Transcript:  entry.Transcript,
		Reply:       entry.Reply,
		LocalOnly:   entry.LocalOnly,
		Pending:     entry.Pending,
	}
	if entry.ResponseMS != nil {
		view.Measured = true
		view.Response = formatMS(*entry.ResponseMS)
	}
	for _, stage := range entry.Stages {
		view.Stages = append(view.Stages, timingView{
			Name:  stage.Name,
			Label: stageLabel(stage.Name),
			MS:    formatMS(stage.MS),
		})
	}
	for _, checkpoint := range entry.Checkpoints {
		view.Checkpoints = append(view.Checkpoints, timingView{
			Name:  checkpoint.Name,
			Label: checkpointLabel(checkpoint.Name),
			MS:    formatMS(checkpoint.MS),
		})
	}
	for _, call := range entry.Calls {
		view.Calls = append(view.Calls, callView{
			Name:      call.Name,
			LocalOnly: call.LocalOnly,
			Result:    call.Result,
		})
	}
	return view
}

func activitySourceLabel(source string) string {
	switch source {
	case "voice":
		return "Voice"
	case "debug":
		return "Debug typed"
	case "dashboard":
		return "Dashboard typed"
	case "dashboard_mic":
		return "Dashboard mic"
	default:
		if source == "" {
			return "Unknown"
		}
		return source
	}
}

func pathLabel(path string) string {
	switch path {
	case "fast_path":
		return "Fast path"
	case "agent_path":
		return "Agent path"
	case "control_phrase":
		// A Control phrase is part of the Fast path; the suffix keeps the
		// exact route visible without hiding that it never waited on an Agent.
		return "Fast path · Control phrase"
	case "confirmation":
		return "Fast path · Confirmation"
	case "focus_intent":
		return "Focus intent"
	case "fallthrough":
		return "Fallthrough"
	default:
		if path == "" {
			return "Unrouted"
		}
		return path
	}
}

func outcomeLabel(outcome string) string {
	switch outcome {
	case "ok":
		return "OK"
	case "no_audio":
		return "No audio"
	case "failed":
		return "Failed"
	case "cancelled":
		return "Cancelled"
	default:
		if outcome == "" {
			return "Unknown"
		}
		return outcome
	}
}

func outcomePill(outcome string) string {
	switch outcome {
	case "ok":
		return "pill-ok"
	case "failed":
		return "pill-error"
	case "cancelled":
		return "pill-warn"
	default:
		return "pill-neutral"
	}
}

func stageLabel(name string) string {
	switch name {
	case "endpointing":
		return "Endpointing"
	case "stt_finalize":
		return "STT finalize"
	case "routing":
		return "Routing"
	case "tool_execute":
		return "Tool execute"
	case "agent_first_token":
		return "Agent first token"
	case "tts_first_audio":
		return "TTS first audio"
	default:
		return name
	}
}

func checkpointLabel(name string) string {
	switch name {
	case "first_chunk":
		return "First reply chunk"
	default:
		return name
	}
}

func formatMS(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}
