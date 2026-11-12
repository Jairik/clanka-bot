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

func (f *activityFixture) page(target string) string {
	f.t.Helper()
	rec := serve(f.handler, request(http.MethodGet, target, owner))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET %s = %d, want 200", target, rec.Code)
	}
	return rec.Body.String()
}

func requestIDFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	location := rec.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("Location %q: %v", location, err)
	}
	id := parsed.Query().Get("request")
	if id == "" {
		t.Fatalf("Location %q has no request id", location)
	}
	return id
}

// TestActivityTypedFastPathShowsRouteToolLatencyAndReply is the core HTTP
// journey: the box posts to the normal pipeline, the reply appears, and the
// Activity row carries the real route, Tool and stage timings without an
// invented Response latency for a text-only request.
func TestActivityTypedFastPathShowsRouteToolLatencyAndReply(t *testing.T) {
	f := newActivityFixture(t)
	rec := f.ask("what time is it")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d, body = %s", rec.Code, rec.Body.String())
	}
	id := requestIDFrom(t, rec)
	body := f.page("/activity?request=" + url.QueryEscape(id))

	for _, want := range []string{
		"Reply", "It&#39;s 3:04 PM.", "Fast path", "time.now",
		"Dashboard typed", "No audio", "Routing", "Tool execute",
		"Response latency: Not measured", "Activity",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Response latency: 0 ms") {
		t.Error("a typed request invented a zero Response latency")
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
	rec := f.ask("read my secret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d, body = %s", rec.Code, rec.Body.String())
	}
	id := requestIDFrom(t, rec)
	page := f.page("/activity?request=" + url.QueryEscape(id))
	for _, want := range []string{
		"The launch code is 1234.",
		"Local-only Tool",
		"Local only",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("authenticated page lacks %q", want)
		}
	}

	health := serve(f.api, request(http.MethodGet, "/healthz", ""))
	if strings.Contains(health.Body.String(), "launch code") {
		t.Fatal("public health response leaked the Local-only result")
	}
	healthPage := f.page("/health")
	if strings.Contains(healthPage, "launch code") {
		t.Fatal("Dashboard health page leaked the Local-only result")
	}

	// A later Agent request keeps the Local-only turn in history but must drop
	// it from the one prompt builder, and the Tool itself is never offered.
	f.model.add(activityText("I don't have that."))
	rec = f.ask("ask the agent what did I just read")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("agent POST = %d", rec.Code)
	}
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
	rec := f.ask("what's the weather")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := f.page("/activity?request=" + url.QueryEscape(requestIDFrom(t, rec)))
	for _, want := range []string{
		"weather.current", "That tool is turned off.", "Failed", "Fast path",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
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

	if rec := f.ask("ask the agent send the mail"); rec.Code != http.StatusSeeOther {
		t.Fatalf("first ask = %d", rec.Code)
	}
	pendingPage := f.page("/activity")
	for _, want := range []string{"Agent path", "Confirmation pending", "Confirmation", "mail.send"} {
		if !strings.Contains(pendingPage, want) {
			t.Errorf("pending page lacks %q", want)
		}
	}

	rec := f.ask("yes")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("yes = %d", rec.Code)
	}
	yesPage := f.page("/activity?request=" + url.QueryEscape(requestIDFrom(t, rec)))
	for _, want := range []string{"Sent.", "Confirmation", "mail.send"} {
		if !strings.Contains(yesPage, want) {
			t.Errorf("yes page lacks %q", want)
		}
	}

	if rec := f.ask("ask the agent send the mail"); rec.Code != http.StatusSeeOther {
		t.Fatalf("second ask = %d", rec.Code)
	}
	rec = f.ask("no")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("no = %d", rec.Code)
	}
	noPage := f.page("/activity?request=" + url.QueryEscape(requestIDFrom(t, rec)))
	if !strings.Contains(noPage, "Cancelled.") {
		t.Errorf("no page lacks the cancellation lead-in: %s", noPage)
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
	if rec := f.ask("read my secret"); rec.Code != http.StatusSeeOther {
		t.Fatalf("ask = %d", rec.Code)
	}
	pending := f.page("/activity")
	for _, want := range []string{"Confirmation pending", "secret.read"} {
		if !strings.Contains(pending, want) {
			t.Fatalf("permission tightening page lacks %q:\n%s", want, pending)
		}
	}

	rec := f.ask("yes")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("yes = %d", rec.Code)
	}
	body := f.page("/activity?request=" + url.QueryEscape(requestIDFrom(t, rec)))
	for _, want := range []string{"The launch code is 1234.", "Confirmation", "Local-only Tool"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
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
	if rec := f.ask("what time is it"); rec.Code != http.StatusSeeOther {
		t.Fatalf("typed ask = %d", rec.Code)
	}
	body := f.page("/activity")
	for _, want := range []string{"Voice", "10.0 s", "Not measured", "OK"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestActivityTypedRequestErrorsHelpAndPreserveDraft covers the bounded form
// and the two response modes.
func TestActivityTypedRequestErrorsHelpAndPreserveDraft(t *testing.T) {
	f := newActivityFixture(t)

	// Plain form: 400 with the full page and the rejected text preserved.
	rec := f.ask("   ")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank POST = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Type a request first.") {
		t.Error("blank error missing")
	}
	longText := strings.Repeat("x", 2001)
	rec = f.ask(longText)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long POST = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "2000 characters") || !strings.Contains(rec.Body.String(), longText) {
		t.Error("long error or preserved draft missing")
	}

	// htmx: a 200 fragment with the same alert, never a full page.
	req := f.form("")
	req.Header.Set("HX-Request", "true")
	rec = serve(f.handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx error = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") || !strings.Contains(rec.Body.String(), `id="activity-content"`) {
		t.Error("htmx error was not a content fragment")
	}
	if !strings.Contains(rec.Body.String(), `role="alert"`) {
		t.Error("htmx error lacks an alert")
	}

	// htmx success: a 200 content fragment with the reply, no redirect.
	req = f.form("what time is it")
	req.Header.Set("HX-Request", "true")
	rec = serve(f.handler, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx success = %d", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("htmx success redirected")
	}
	if !strings.Contains(rec.Body.String(), "It&#39;s 3:04 PM.") || !strings.Contains(rec.Body.String(), `id="activity-content"`) {
		t.Errorf("htmx success lacks the reply fragment: %s", rec.Body.String())
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
		if code != http.StatusSeeOther {
			t.Fatalf("request %d status = %d, want 303", i, code)
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
	rec := serve(handler, request(http.MethodGet, "/activity", owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if got := strings.Count(body, "for request req-"); got != 50 {
		t.Fatalf("displayed requests = %d, want 50", got)
	}
	for _, want := range []string{"request req-120", "request req-071"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, absent := range []string{"request req-070", "request req-001"} {
		if strings.Contains(body, absent) {
			t.Errorf("page includes evicted %q", absent)
		}
	}
}

// TestActivityEscapesUserAndToolText keeps both the transcript and a hostile
// Local-only result inert in the page.
func TestActivityEscapesUserAndToolText(t *testing.T) {
	f := newActivityFixture(t)
	rec := f.ask("read the hostile secret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ask = %d", rec.Code)
	}
	body := f.page("/activity?request=" + url.QueryEscape(requestIDFrom(t, rec)))
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("hostile Tool result was not escaped")
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("hostile Tool result rendered as HTML")
	}

	// The transcript is escaped the same way; the model's answer is scripted
	// so this remains a real pipeline turn.
	f.model.add(activityText("I won't run that."))
	rec = f.ask("<b>hostile</b>")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ask = %d", rec.Code)
	}
	body = f.page("/activity?request=" + url.QueryEscape(requestIDFrom(t, rec)))
	if !strings.Contains(body, "&lt;b&gt;hostile&lt;/b&gt;") {
		t.Error("hostile transcript was not escaped")
	}
	if strings.Contains(body, "<b>hostile</b>") {
		t.Error("hostile transcript rendered as HTML")
	}
}
