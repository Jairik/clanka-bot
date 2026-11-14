package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/activity"
	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/audio"
	"github.com/Jairik/clanka-bot/assistant/internal/dashboard"
	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/internal/toolpolicy"
	"github.com/Jairik/clanka-bot/assistant/internal/wording"
)

const dashboardTestKeyEnv = "DASHBOARD_ACTIVITY_TEST_KEY"

// activityModel is a scripted OpenAI-compatible SSE endpoint. It records each
// request body so the privacy test can prove that a Local-only result never
// appears in a model prompt.
type activityModel struct {
	mu      sync.Mutex
	replies []string
	bodies  []string
}

func (m *activityModel) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.bodies = append(m.bodies, string(body))
		reply := ""
		if len(m.replies) > 0 {
			reply = m.replies[0]
			m.replies = m.replies[1:]
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server
}

func (m *activityModel) add(replies ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies = append(m.replies, replies...)
}

func (m *activityModel) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

func activityText(text string) string {
	return activitySSE(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}, "stop")
}

func activityToolCall(name, args string) string {
	return activitySSE(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index":    0,
					"id":       "call_test",
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			},
			"finish_reason": nil,
		}},
	}, "tool_calls")
}

func activitySSE(chunk map[string]any, finish string) string {
	chunk["id"] = "chatcmpl-test"
	chunk["object"] = "chat.completion.chunk"
	chunk["created"] = 1
	chunk["model"] = "test"
	raw, _ := json.Marshal(chunk)
	done, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finish,
		}},
	})
	return "data: " + string(raw) + "\n\ndata: " + string(done) + "\n\ndata: [DONE]\n\n"
}

// activitySink is a fake audio sink. A typed request must never open it.
type activitySink struct {
	mu    sync.Mutex
	opens int
}

func (s *activitySink) Open(context.Context, audio.Format) (io.WriteCloser, error) {
	s.mu.Lock()
	s.opens++
	s.mu.Unlock()
	return nopWriteCloser{}, nil
}

// nopWriteCloser satisfies the fake sink's writer without touching a device.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func (s *activitySink) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// activityFixture wires the real registry, policy, settings, pipeline and
// reply Session, with a scripted model and a fake audio sink. Only the model
// and audio are fake.
type activityFixture struct {
	t      *testing.T
	ctx    context.Context
	logger *slog.Logger
	store  *activity.Store
	model  *activityModel
	sink   *activitySink
	riser  *activityRiser
	reg    *tool.Registry
	policy *toolpolicy.Policy
	sent   int

	pipeline *pipeline.Pipeline
	api      http.Handler
	handler  *dashboard.Handler
}

// activityRiser tracks concurrent Tool executions so overlapping typed
// requests can prove they were serialised, not interleaved.
type activityRiser struct {
	mu      sync.Mutex
	current int
	max     int
	runs    int
}

func (r *activityRiser) enter() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++
	r.current++
	if r.current > r.max {
		r.max = r.current
	}
}

func (r *activityRiser) leave() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current--
}

func (r *activityRiser) maxConcurrent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := &activityFixture{
		t:      t,
		ctx:    ctx,
		logger: logger,
		store:  activity.New(activity.DefaultLimit),
		model:  &activityModel{},
		sink:   &activitySink{},
		riser:  &activityRiser{},
		reg:    tool.NewRegistry(2 * time.Second),
	}
	add := func(def tool.Tool[struct{}, string]) {
		t.Helper()
		if err := tool.Add(f.reg, def); err != nil {
			t.Fatalf("register %s: %v", def.Name, err)
		}
	}
	add(tool.Tool[struct{}, string]{
		Name: "time.now", Description: "Get the current time.",
		Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			time.Sleep(15 * time.Millisecond)
			return "3:04 PM", nil
		},
		Speak: func(string) string { return "It's 3:04 PM." },
	})
	add(tool.Tool[struct{}, string]{
		Name: "secret.read", Description: "Read a local secret.",
		Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (string, error) {
			return "the launch code is 1234", nil
		},
		Speak: func(string) string { return "The launch code is 1234." },
	})
	add(tool.Tool[struct{}, string]{
		Name: "weather.current", Description: "Read the weather.",
		Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) { return "sunny", nil },
		Speak:   func(string) string { return "It's sunny." },
	})
	add(tool.Tool[struct{}, string]{
		Name: "mail.send", Description: "Send the mail.",
		Permission: tool.PermissionConfirm, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			// One accepted Confirmation runs the handler exactly once.
			f.sent++
			return "sent", nil
		},
		Speak: func(string) string { return "Sent." },
	})
	add(tool.Tool[struct{}, string]{
		Name: "secret.hostile", Description: "Read a hostile local secret.",
		Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (string, error) {
			return `<script>alert(1)</script> & "quoted"`, nil
		},
		Speak: func(string) string { return `<script>alert(1)</script> & "quoted"` },
	})
	add(tool.Tool[struct{}, string]{
		Name: "test.slow", Description: "Run a slow local Tool.",
		Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			f.riser.enter()
			time.Sleep(80 * time.Millisecond)
			f.riser.leave()
			return "slow done", nil
		},
		Speak: func(string) string { return "Slow done." },
	})

	setup, err := wording.Build(f.reg, wording.Config{
		ControlPhrases: map[string]string{
			"what time is it":         "time.now",
			"read my secret":          "secret.read",
			"what's the weather":      "weather.current",
			"run the slow tool":       "test.slow",
			"read the hostile secret": "secret.hostile",
		},
		RequestPrefixes:    []string{"ask the agent"},
		Filler:             "One moment.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
	})
	if err != nil {
		t.Fatalf("wording.Build: %v", err)
	}
	toolDefs, err := toolpolicy.Definitions(f.reg, toolpolicy.Config{Disabled: []string{"weather.current"}})
	if err != nil {
		t.Fatalf("toolpolicy.Definitions: %v", err)
	}
	defs := append([]settings.Definition{}, toolDefs...)
	defs = append(defs, setup.Definitions...)
	svc, err := settings.New(ctx, st, defs...)
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	f.policy, err = toolpolicy.New(svc, f.reg)
	if err != nil {
		t.Fatalf("toolpolicy.New: %v", err)
	}

	modelServer := f.model.start(t)
	t.Setenv(dashboardTestKeyEnv, "test-key")
	agentPath, err := agent.New(agent.Settings{
		BaseURL:            modelServer.URL,
		APIKeyEnv:          dashboardTestKeyEnv,
		Model:              "test/model",
		SystemPrompt:       "Test system prompt.",
		UnavailableMessage: "Sorry, I can't answer that right now.",
		FirstTokenTimeout:  2 * time.Second,
		RequestTimeout:     5 * time.Second,
		MaxSteps:           3,
		HistoryTurns:       10,
	}, f.reg, logger)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	f.pipeline, err = pipeline.New(f.reg, map[string]string{
		"what time is it":         "time.now",
		"read my secret":          "secret.read",
		"what's the weather":      "weather.current",
		"run the slow tool":       "test.slow",
		"read the hostile secret": "secret.hostile",
	}, logger,
		pipeline.WithAgent(agentPath, []string{"ask the agent"}),
		pipeline.WithHistory(agentPath.History()),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(f.reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	// A text-only Session with a fake sink: it is the same Ask path main uses
	// for the Dashboard, and opening the sink would be a real regression.
	typed := reply.New(f.pipeline, nil, f.sink, nil, reply.WithLogger(logger))
	f.handler = newHandler(t, dashboard.Options{
		Logger:   logger,
		Activity: f.store,
		Typed:    typed,
	})
	f.api = api.NewHandler(f.pipeline, logger, false)
	return f
}

func (f *activityFixture) form(text string) *http.Request {
	f.t.Helper()
	values := url.Values{"text": {text}}
	req := httptest.NewRequest(http.MethodPost, "/activity/ask", strings.NewReader(values.Encode()))
	req.Host = "127.0.0.1:8082"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Tailscale-User-Login", owner)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func (f *activityFixture) ask(text string) *httptest.ResponseRecorder {
	f.t.Helper()
	return serve(f.handler, f.form(text))
}

// activityRow and activityResp decode the parts of the Activity view the
// tests assert on. Field names are the Go view struct's (encoding/json default).
type activityRow struct {
	ID          string
	Source      string
	SourceLabel string
	Path        string
	PathLabel   string
	Tool        string
	Outcome     string
	OutcomeText string
	Response    string
	Measured    bool
	Transcript  string
	Reply       string
	LocalOnly   bool
	Pending     bool
	Stages      []struct{ Name, Label, MS string }
	Calls       []struct {
		Name      string
		LocalOnly bool
		Result    string
	}
}

type activityResp struct {
	Error         string
	Notice        string
	Draft         string
	HasActivity   bool
	HasTyped      bool
	DisplayLimit  int
	Just          *activityRow
	Requests      []activityRow
	JustRequestID string
}

// view reads the Activity JSON, failing unless it is a 200.
func (f *activityFixture) view(target string) (activityResp, string) {
	f.t.Helper()
	var v activityResp
	rec := getJSON(f.t, f.handler, target, &v)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET %s = %d, want 200: %s", target, rec.Code, rec.Body.String())
	}
	return v, rec.Body.String()
}

// mustAsk submits a typed request that must succeed (200) and returns the
// refreshed view, whose Just row is the request just answered.
func (f *activityFixture) mustAsk(text string) (activityResp, string) {
	f.t.Helper()
	rec := f.ask(text)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("POST %q = %d, body = %s", text, rec.Code, rec.Body.String())
	}
	var v activityResp
	decodeJSON(f.t, rec, &v)
	if v.Error != "" {
		f.t.Fatalf("POST %q answered 200 with error %q", text, v.Error)
	}
	if v.Just == nil || v.Just.ID == "" || v.Just.ID != v.JustRequestID {
		f.t.Fatalf("POST %q Just = %+v, JustRequestID = %q", text, v.Just, v.JustRequestID)
	}
	return v, rec.Body.String()
}

// requireRow fails unless the row's encoded fields contain every want.
func requireRow(t *testing.T, row *activityRow, wants ...string) {
	t.Helper()
	if row == nil {
		t.Fatal("no request row")
	}
	raw, _ := json.Marshal(row)
	text := string(raw)
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("row lacks %q: %s", want, text)
		}
	}
}

func stageLabels(row *activityRow) []string {
	var labels []string
	for _, stage := range row.Stages {
		labels = append(labels, stage.Label)
	}
	return labels
}

// TestActivityTypedFastPathShowsRouteToolLatencyAndReply is the core HTTP
// journey: the box posts to the normal pipeline, the reply appears, and the
// Activity row carries the real route, Tool and stage timings without an
// invented Response latency for a text-only request.
func TestActivityTypedFastPathShowsRouteToolLatencyAndReply(t *testing.T) {
	f := newActivityFixture(t)
	posted, _ := f.mustAsk("what time is it")
	id := posted.Just.ID
	// The same request is reachable by id on a plain read, as the page does
	// after a reload.
	v, _ := f.view("/api/ui/activity?request=" + url.QueryEscape(id))
	if v.Just == nil || v.Just.ID != id {
		t.Fatalf("?request=%s Just = %+v", id, v.Just)
	}
	row := v.Just

	if row.Reply != "It's 3:04 PM." || row.Transcript != "what time is it" {
		t.Errorf("transcript/reply = %q / %q", row.Transcript, row.Reply)
	}
	if row.Path != "control_phrase" || !strings.Contains(row.PathLabel, "Fast path") {
		t.Errorf("path = %q (%q)", row.Path, row.PathLabel)
	}
	if row.Tool != "time.now" || row.SourceLabel != "Dashboard typed" || row.OutcomeText != "No audio" {
		t.Errorf("tool/source/outcome = %q / %q / %q", row.Tool, row.SourceLabel, row.OutcomeText)
	}
	if row.Measured || row.Response != "Not measured" {
		t.Errorf("a typed request invented a Response latency: measured=%v response=%q", row.Measured, row.Response)
	}
	labels := strings.Join(stageLabels(row), ",")
	for _, want := range []string{"Routing", "Tool execute"} {
		if !strings.Contains(labels, want) {
			t.Errorf("stages %q lack %q", labels, want)
		}
	}
	if calls := f.sink.openCount(); calls != 0 {
		t.Fatalf("typed request opened the audio sink %d times, want 0", calls)
	}
	if entry, ok := f.store.Get(id); !ok || entry.Path != "control_phrase" || entry.ResponseMS != nil {
		t.Fatalf("stored entry = %+v, ok=%v", entry, ok)
	}
}

// TestActivityTypedLocalOnlyResultStaysOnTheDashboard proves the acceptance
// privacy rule end to end: the result is visible in the authenticated page,
// absent from the public health response, absent from the shared Agent prompt
// and absent from the model's Tool list.
func TestActivityTypedLocalOnlyResultStaysOnTheDashboard(t *testing.T) {
	f := newActivityFixture(t)
	posted, _ := f.mustAsk("read my secret")
	id := posted.Just.ID
	row := posted.Just
	if row.Reply != "The launch code is 1234." || !row.LocalOnly {
		t.Errorf("row = %+v, want the visible Local-only reply", row)
	}
	if len(row.Calls) != 1 || !row.Calls[0].LocalOnly || !strings.Contains(row.Calls[0].Result, "1234") {
		t.Errorf("calls = %+v, want one Local-only call with its result", row.Calls)
	}

	health := serve(f.api, request(http.MethodGet, "/healthz", ""))
	if strings.Contains(health.Body.String(), "launch code") {
		t.Fatal("public health response leaked the Local-only result")
	}
	healthRec := serve(f.handler, request(http.MethodGet, "/api/ui/health", owner))
	if strings.Contains(healthRec.Body.String(), "launch code") {
		t.Fatal("Dashboard health JSON leaked the Local-only result")
	}

	// A later Agent request keeps the Local-only turn in history but must drop
	// it from the one prompt builder, and the Tool itself is never offered.
	f.model.add(activityText("I don't have that."))
	f.mustAsk("ask the agent what did I just read")
	requests := f.model.requests()
	if len(requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(requests))
	}
	for _, leaked := range []string{"launch code", "secret.read"} {
		if strings.Contains(requests[0], leaked) {
			t.Fatalf("model prompt leaked %q:\n%s", leaked, requests[0])
		}
	}

	// The next request still shows in Activity, and the Local-only entry is
	// still there; nothing was rewritten by the model path.
	if _, ok := f.store.Get(id); !ok {
		t.Fatal("Local-only entry disappeared from Activity")
	}
}

// TestActivityTypedDisabledToolShowsHelpfulOutcome covers a live Tool-policy
// disable reached through a normal Control phrase.
func TestActivityTypedDisabledToolShowsHelpfulOutcome(t *testing.T) {
	f := newActivityFixture(t)
	posted, _ := f.mustAsk("what's the weather")
	requireRow(t, posted.Just, "weather.current", "That tool is turned off.", "Failed", "Fast path")
}

// TestActivityTypedConfirmationYesAndNo drives the real Agent loop with a
// scripted model: the first typed turn asks a Confirmation, "yes" runs the
// confirm Tool exactly once, and "no" drops a later pending call.
func TestActivityTypedConfirmationYesAndNo(t *testing.T) {
	f := newActivityFixture(t)
	f.model.add(
		activityToolCall("mail__send", `{}`),
		activityToolCall("mail__send", `{}`),
		activityText("Okay."),
	)

	pending, _ := f.mustAsk("ask the agent send the mail")
	requireRow(t, pending.Just, "Agent path", "mail.send")
	if !pending.Just.Pending {
		t.Errorf("first turn Pending = false, want a pending Confirmation: %+v", pending.Just)
	}

	yes, _ := f.mustAsk("yes")
	requireRow(t, yes.Just, "Sent.", "Confirmation", "mail.send")

	f.mustAsk("ask the agent send the mail")
	no, _ := f.mustAsk("no")
	if !strings.Contains(no.Just.Reply, "Cancelled.") {
		t.Errorf("no reply lacks the cancellation lead-in: %+v", no.Just)
	}
	// Only the clear yes may have run the outward-facing handler.
	if f.sent != 1 {
		t.Fatalf("mail.send ran %d times, want 1", f.sent)
	}
}

// TestActivityTypedPermissionTighteningConfirmsAndKeepsLocalOnly tightens a
// Fast path Tool to confirm through the real policy, then settles it with a
// typed yes. The result stays Local-only and is visible in the Dashboard only.
func TestActivityTypedPermissionTighteningConfirmsAndKeepsLocalOnly(t *testing.T) {
	f := newActivityFixture(t)
	confirm := tool.PermissionConfirm
	if _, err := f.policy.Set(f.ctx, "secret.read", toolpolicy.Update{Permission: &confirm}); err != nil {
		t.Fatalf("tighten secret.read: %v", err)
	}
	pending, _ := f.mustAsk("read my secret")
	if !pending.Just.Pending {
		t.Fatalf("permission tightening did not leave a pending Confirmation: %+v", pending.Just)
	}
	requireRow(t, pending.Just, "secret.read")

	yes, _ := f.mustAsk("yes")
	requireRow(t, yes.Just, "The launch code is 1234.", "Confirmation")
	if !yes.Just.LocalOnly {
		t.Errorf("confirmed result lost its Local-only flag: %+v", yes.Just)
	}
}

// TestActivityMeasuredResponseLatencyComesOnlyFromReplyAudio shows the honest
// latency semantics: a voice completion with real reply-audio marks displays a
// measured Response latency, while typed rows say Not measured.
func TestActivityMeasuredResponseLatencyComesOnlyFromReplyAudio(t *testing.T) {
	f := newActivityFixture(t)
	timeline := latency.New("voice-1", latency.SourceVoice, latency.WithObserver(f.store.Observe))
	ctx := latency.WithContext(context.Background(), timeline)
	timeline.SetLastVoice(time.Now().Add(-10 * time.Second))
	latency.SetRoute(ctx, "fast_path", "time.now", "what time is it")
	latency.SetStage(ctx, latency.StageRouting, 40*time.Millisecond)
	latency.SetStage(ctx, latency.StageTTSFirstAudio, 120*time.Millisecond)
	latency.SetSummary(ctx, "It's 3:04 PM.", false, false)
	timeline.MarkReplyAudio()
	timeline.Complete(f.logger, latency.OutcomeOK, nil)

	// And one typed turn, which has no speech start or reply audio.
	f.mustAsk("what time is it")
	v, _ := f.view("/api/ui/activity")
	bySource := map[string]activityRow{}
	for _, row := range v.Requests {
		bySource[row.Source] = row
	}
	voice, typed := bySource["voice"], bySource["dashboard"]
	if !voice.Measured || voice.Response != "10.0 s" || voice.OutcomeText != "OK" || voice.SourceLabel != "Voice" {
		t.Errorf("voice row = %+v, want a measured 10.0 s Response latency", voice)
	}
	if typed.Measured || typed.Response != "Not measured" {
		t.Errorf("typed row = %+v, want Not measured", typed)
	}
}

// TestActivityTypedRequestErrorsHelpAndPreserveDraft covers the bounded form:
// a failure answers the view as JSON with the real status code, the error text
// and the rejected draft.
func TestActivityTypedRequestErrorsHelpAndPreserveDraft(t *testing.T) {
	f := newActivityFixture(t)

	rec := f.ask("   ")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank POST = %d, want 400", rec.Code)
	}
	var v activityResp
	decodeJSON(t, rec, &v)
	if v.Error != "Type a request first." || v.Just != nil {
		t.Errorf("blank view = error %q just %+v", v.Error, v.Just)
	}

	longText := strings.Repeat("x", 2001)
	rec = f.ask(longText)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long POST = %d, want 400", rec.Code)
	}
	v = activityResp{}
	decodeJSON(t, rec, &v)
	if !strings.Contains(v.Error, "2000 characters") || v.Draft != longText {
		t.Errorf("long view error = %q, draft preserved = %v", v.Error, v.Draft == longText)
	}
	if f.store.Len() != 0 {
		t.Errorf("rejected requests reached the pipeline: %d entries", f.store.Len())
	}
}

// TestActivityTypedSuccessAnswersJSONWithoutARedirect pins the write contract
// the React app relies on: 200, the refreshed view, no redirect.
func TestActivityTypedSuccessAnswersJSONWithoutARedirect(t *testing.T) {
	f := newActivityFixture(t)
	rec := f.ask("what time is it")
	if rec.Code != http.StatusOK {
		t.Fatalf("success = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("success redirected")
	}
	var v activityResp
	decodeJSON(t, rec, &v)
	if v.Just == nil || v.Just.Reply != "It's 3:04 PM." || len(v.Requests) != 1 || v.Draft != "" {
		t.Errorf("view = %+v", v)
	}
}

// TestActivityTypedRequestRequiresIdentityAndSameOrigin repeats the shared
// boundary for the one new write route.
func TestActivityTypedRequestRequiresIdentityAndSameOrigin(t *testing.T) {
	f := newActivityFixture(t)
	for name, mutate := range map[string]func(*http.Request){
		"no identity": func(r *http.Request) { r.Header.Del("Tailscale-User-Login") },
		"no origin":   func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"cross origin": func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
			r.Header.Set("Origin", "https://evil.example")
		},
		"cross fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		t.Run(name, func(t *testing.T) {
			before := f.store.Len()
			req := f.form("what time is it")
			mutate(req)
			rec := serve(f.handler, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if f.store.Len() != before {
				t.Fatal("rejected request ran the pipeline")
			}
		})
	}
}

// TestActivityTypedOverlappingRequestsSerialize proves the one-slot guard:
// two concurrent submissions both finish, but the Tool handler never runs
// twice at once.
func TestActivityTypedOverlappingRequestsSerialize(t *testing.T) {
	f := newActivityFixture(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serve(f.handler, f.form("run the slow tool")).Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, code)
		}
	}
	if got := f.riser.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent Tool runs = %d, want 1 (serialised)", got)
	}
	if got := f.store.Len(); got != 2 {
		t.Fatalf("activity entries = %d, want 2", got)
	}
}

// TestActivityHistoryIsBoundedToTheDisplayWindow seeds more requests than the
// page shows and proves the oldest are absent.
func TestActivityHistoryIsBoundedToTheDisplayWindow(t *testing.T) {
	store := activity.New(100)
	for i := 1; i <= 120; i++ {
		store.Observe(latency.Completed{
			ID:         fmt.Sprintf("req-%03d", i),
			Source:     latency.SourceVoice,
			Path:       "fast_path",
			Transcript: "request",
			Outcome:    latency.OutcomeOK,
		})
	}
	handler := newHandler(t, dashboard.Options{Activity: store})
	var v activityResp
	rec := getJSON(t, handler, "/api/ui/activity", &v)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(v.Requests) != 50 || v.DisplayLimit != 50 {
		t.Fatalf("displayed requests = %d (limit %d), want 50", len(v.Requests), v.DisplayLimit)
	}
	ids := map[string]bool{}
	for _, row := range v.Requests {
		ids[row.ID] = true
	}
	for _, want := range []string{"req-120", "req-071"} {
		if !ids[want] {
			t.Errorf("view lacks %q", want)
		}
	}
	for _, absent := range []string{"req-070", "req-001"} {
		if ids[absent] {
			t.Errorf("view includes evicted %q", absent)
		}
	}
}

// TestActivityHostileUserAndToolTextRoundTripsInertly keeps both the
// transcript and a hostile Local-only result exact in the JSON and free of a
// raw script tag, so a client that injected the body would still be inert.
func TestActivityHostileUserAndToolTextRoundTripsInertly(t *testing.T) {
	f := newActivityFixture(t)
	posted, body := f.mustAsk("read the hostile secret")
	const hostile = `<script>alert(1)</script> & "quoted"`
	if posted.Just.Reply != hostile {
		t.Errorf("hostile Tool reply = %q, want it unchanged", posted.Just.Reply)
	}
	noRawScript(t, body)

	// The model's answer is scripted so this remains a real pipeline turn.
	f.model.add(activityText("I won't run that."))
	posted, body = f.mustAsk("<b>hostile</b>")
	if posted.Just.Transcript != "<b>hostile</b>" {
		t.Errorf("hostile transcript = %q, want it unchanged", posted.Just.Transcript)
	}
	if strings.Contains(body, "<b>") {
		t.Errorf("body carries a raw tag: %s", body)
	}
}
