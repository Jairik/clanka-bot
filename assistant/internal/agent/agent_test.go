package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

const testKeyEnv = "AGENT_TEST_API_KEY"

// fakeProvider is a scripted OpenAI-compatible endpoint. It records every
// request body and header, then replies with the next scripted SSE payload.
type fakeProvider struct {
	t *testing.T

	mu       sync.Mutex
	requests []fakeRequest
	replies  []string
	status   int
	delay    time.Duration
}

type fakeRequest struct {
	Body   map[string]any
	Header http.Header
}

func newFakeProvider(t *testing.T, replies ...string) *fakeProvider {
	t.Helper()
	return &fakeProvider{t: t, replies: replies}
}

func (f *fakeProvider) start() *httptest.Server {
	f.t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(server.Close)
	return server
}

func (f *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake provider: decode request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Body: body, Header: r.Header.Clone()})
	status := f.status
	delay := f.delay
	reply := ""
	if len(f.replies) > 0 {
		reply = f.replies[0]
		f.replies = f.replies[1:]
	}
	f.mu.Unlock()

	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":"scripted failure","type":"test"}}`)
		return
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, reply)
}

func (f *fakeProvider) request(t *testing.T, index int) fakeRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.requests) {
		t.Fatalf("fake provider: request %d missing (got %d)", index, len(f.requests))
	}
	return f.requests[index]
}

func (f *fakeProvider) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type fakeCall struct {
	ID   string
	Name string
	Args string
}

// textReply renders a streamed assistant text answer split into the given parts.
func textReply(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		chunk := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": part},
				"finish_reason": nil,
			}},
		}
		writeChunk(&b, chunk)
	}
	writeFinish(&b, "stop")
	return b.String()
}

// toolCallReply renders a streamed tool-call turn.
func toolCallReply(content string, calls ...fakeCall) string {
	var b strings.Builder
	deltas := make([]any, 0, len(calls))
	for i, call := range calls {
		deltas = append(deltas, map[string]any{
			"index": i,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Args,
			},
		})
	}
	chunk := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": content, "tool_calls": deltas},
			"finish_reason": nil,
		}},
	}
	writeChunk(&b, chunk)
	writeFinish(&b, "tool_calls")
	return b.String()
}

func writeChunk(b *strings.Builder, chunk map[string]any) {
	raw, err := json.Marshal(chunk)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(b, "data: %s\n\n", raw)
}

func writeFinish(b *strings.Builder, reason string) {
	chunk := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	writeChunk(b, chunk)
	b.WriteString("data: [DONE]\n\n")
}

func testSettings(serverURL string) agent.Settings {
	return agent.Settings{
		BaseURL:            serverURL,
		APIKeyEnv:          testKeyEnv,
		Model:              "deepseek/deepseek-v4.1-flash",
		Routing:            map[string]any{"sort": "latency"},
		UnavailableMessage: "Sorry, I can't answer that right now.",
		FirstTokenTimeout:  2 * time.Second,
		RequestTimeout:     5 * time.Second,
	}
}

func newTestAgent(t *testing.T, settings agent.Settings, registry *tool.Registry, logger *slog.Logger) *agent.Agent {
	t.Helper()
	t.Setenv(testKeyEnv, "test-key")
	if registry == nil {
		registry = newRegistry(t)
	}
	a, err := agent.New(settings, registry, logger)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func newRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(time.Second)
	if err := clock.Register(registry, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	return registry
}

func addTool[In, Out any](t *testing.T, registry *tool.Registry, def tool.Tool[In, Out]) {
	t.Helper()
	if err := tool.Add(registry, def); err != nil {
		t.Fatalf("register %s: %v", def.Name, err)
	}
}

func TestRequestCarriesPrivacyAndProviderFields(t *testing.T) {
	fake := newFakeProvider(t, textReply("Hello."))
	server := fake.start()
	settings := testSettings(server.URL)
	settings.Headers = map[string]string{"X-Provider-Session": "session-1"}
	// A hostile config must not be able to relax the privacy fields.
	settings.Routing = map[string]any{
		"sort":            "latency",
		"zdr":             false,
		"data_collection": "allow",
	}

	a := newTestAgent(t, settings, nil, nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-1"}, "hello there", nil)
	if result.Text != "Hello." {
		t.Fatalf("reply = %q, want %q", result.Text, "Hello.")
	}

	request := fake.request(t, 0)
	if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want bearer key from the env var", got)
	}
	if got := request.Header.Get("X-Provider-Session"); got != "session-1" {
		t.Errorf("extra header = %q, want session-1", got)
	}
	if got := request.Body["model"]; got != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("model = %v, want deepseek/deepseek-v4.1-flash", got)
	}
	provider, ok := request.Body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider field missing from payload: %v", request.Body)
	}
	if provider["zdr"] != true {
		t.Errorf("provider.zdr = %v, want true even when config says false", provider["zdr"])
	}
	if provider["data_collection"] != "deny" {
		t.Errorf("provider.data_collection = %v, want deny even when config says allow", provider["data_collection"])
	}
	if provider["sort"] != "latency" {
		t.Errorf("provider.sort = %v, want latency", provider["sort"])
	}
}

func TestModelFuncAppliesToTheNextRequest(t *testing.T) {
	fake := newFakeProvider(t, textReply("one"), textReply("two"))
	server := fake.start()
	settings := testSettings(server.URL)
	// The resolver stands in for the live settings service: reading it before
	// every request is what makes a Dashboard edit apply without a restart.
	var mu sync.Mutex
	model := "openrouter/one"
	settings.ModelFunc = func() string {
		mu.Lock()
		defer mu.Unlock()
		return model
	}
	a := newTestAgent(t, settings, nil, nil)

	if result := a.Run(context.Background(), tool.Request{ID: "req-model-1"}, "first", nil); result.Text != "one" {
		t.Fatalf("first reply = %q, want one", result.Text)
	}
	mu.Lock()
	model = "openrouter/two"
	mu.Unlock()
	if result := a.Run(context.Background(), tool.Request{ID: "req-model-2"}, "second", nil); result.Text != "two" {
		t.Fatalf("second reply = %q, want two", result.Text)
	}

	if got := fake.request(t, 0).Body["model"]; got != "openrouter/one" {
		t.Errorf("first request model = %v, want openrouter/one", got)
	}
	if got := fake.request(t, 1).Body["model"]; got != "openrouter/two" {
		t.Errorf("second request model = %v, want openrouter/two", got)
	}
}

func TestModelSettingOverrideAppliesToTheNextRequest(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "model-live.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	svc, err := settings.New(ctx, st, agent.ModelDefinition("first/model", "first/model"))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}

	fake := newFakeProvider(t, textReply("one"), textReply("two"))
	candidate := testSettings(fake.start().URL)
	candidate.Model = "first/model"
	// This is the same resolver main installs: the setting is read before the
	// next provider request, not frozen when the Agent path is built.
	candidate.ModelFunc = func() string { return svc.String(agent.ModelKey) }
	a := newTestAgent(t, candidate, nil, nil)

	a.Run(ctx, tool.Request{ID: "req-live-1"}, "first", nil)
	if _, err := svc.Set(ctx, agent.ModelKey, "second/model"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	a.Run(ctx, tool.Request{ID: "req-live-2"}, "second", nil)

	if got := fake.request(t, 0).Body["model"]; got != "first/model" {
		t.Errorf("first request model = %v", got)
	}
	if got := fake.request(t, 1).Body["model"]; got != "second/model" {
		t.Errorf("second request model = %v, want the stored override", got)
	}
}

func TestModelDefinitionCarriesTheConfigLayerAndRejectsBlank(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "model.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	svc, err := settings.New(ctx, st, agent.ModelDefinition("code/model", "config/model"))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	value, ok := svc.Get(agent.ModelKey)
	if !ok {
		t.Fatalf("%s is not defined", agent.ModelKey)
	}
	if value.Value != "config/model" || value.Source != settings.SourceConfig {
		t.Errorf("effective model = %v from %q, want config/model from config", value.Value, value.Source)
	}
	if value.Kind != settings.Live {
		t.Errorf("kind = %q, want live", value.Kind)
	}
	if _, err := svc.SetJSON(ctx, agent.ModelKey, json.RawMessage(`"   "`)); err == nil {
		t.Fatal("a blank model override was accepted")
	}
}

func TestAgentCallsToolAndUsesResult(t *testing.T) {
	fake := newFakeProvider(t,
		toolCallReply("", fakeCall{ID: "call-1", Name: "time__now", Args: "{}"}),
		textReply("It's 3:04 PM."),
	)
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-2"}, "what time is it", nil)
	if result.Text != "It's 3:04 PM." {
		t.Fatalf("reply = %q, want the model's final answer", result.Text)
	}
	if len(result.Calls) != 1 || result.Calls[0].Name != "time.now" {
		t.Fatalf("recorded calls = %+v, want one time.now call", result.Calls)
	}
	if fake.requestCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", fake.requestCount())
	}

	first := fake.request(t, 0)
	tools, _ := first.Body["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("first request offered no tools")
	}
	found := false
	for _, raw := range tools {
		entry := raw.(map[string]any)
		fn := entry["function"].(map[string]any)
		if fn["name"] == "time__now" {
			found = true
			if params, ok := fn["parameters"].(map[string]any); !ok || params["type"] != "object" {
				t.Errorf("time__now parameters = %v, want an object schema", fn["parameters"])
			}
		}
	}
	if !found {
		t.Fatal("time__now was not offered to the model")
	}

	second := fake.request(t, 1)
	if !strings.Contains(bodyString(t, second.Body), `"role":"tool"`) {
		t.Fatalf("second request has no tool result: %s", bodyString(t, second.Body))
	}
	if !strings.Contains(bodyString(t, second.Body), "2026-11-11T15:04:00Z") {
		t.Fatalf("second request is missing the time.now result: %s", bodyString(t, second.Body))
	}
}

func TestLocalOnlyToolIsNeverOfferedOrCalled(t *testing.T) {
	called := 0
	registry := newRegistry(t)
	addTool(t, registry, tool.Tool[struct{}, struct {
		Subject string `json:"subject"`
	}]{
		Name:        "mail.inbox",
		Description: "Read the inbox.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (struct {
			Subject string `json:"subject"`
		}, error) {
			called++
			return struct {
				Subject string `json:"subject"`
			}{Subject: "secret doctor appointment"}, nil
		},
	})

	fake := newFakeProvider(t,
		toolCallReply("", fakeCall{ID: "call-1", Name: "mail__inbox", Args: "{}"}),
		textReply("I can't read that."),
	)
	a := newTestAgent(t, testSettings(fake.start().URL), registry, nil)

	// A Local-only turn stays in history on the Pi.
	a.History().Add(agent.Turn{
		Utterance: "read my inbox",
		Reply:     "You have one email.",
		Calls: []agent.ToolCall{{
			Name:      "mail.inbox",
			Result:    json.RawMessage(`{"subject":"secret doctor appointment"}`),
			LocalOnly: true,
		}},
	})

	if got := a.Run(context.Background(), tool.Request{ID: "req-3"}, "and tomorrow?", nil); got.Text == "" {
		t.Fatal("empty reply")
	}
	if called != 0 {
		t.Fatalf("Local-only handler ran %d times, want 0", called)
	}

	for i := 0; i < fake.requestCount(); i++ {
		body := fake.request(t, i)
		if offersTool(t, body.Body, "mail__inbox") {
			t.Errorf("request %d offered a Local-only tool", i)
		}
		raw := bodyString(t, body.Body)
		if strings.Contains(raw, "secret doctor appointment") || strings.Contains(raw, "read my inbox") {
			t.Errorf("request %d carried Local-only history: %s", i, raw)
		}
	}
}

func TestConfirmToolNeverExecutes(t *testing.T) {
	calls := 0
	registry := newRegistry(t)
	addTool(t, registry, tool.Tool[struct{}, struct{}]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Summary:     func(struct{}) string { return "Send the email to the doctor?" },
		Handler: func(context.Context, struct{}) (struct{}, error) {
			calls++
			return struct{}{}, nil
		},
	})

	fake := newFakeProvider(t, toolCallReply("", fakeCall{ID: "call-1", Name: "mail__send", Args: "{}"}))
	a := newTestAgent(t, testSettings(fake.start().URL), registry, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-4"}, "email my doctor", nil)
	if calls != 0 {
		t.Fatalf("confirm handler ran %d times, want 0 before the spoken yes", calls)
	}
	if result.Pending == nil || result.Pending.Tool != "mail.send" {
		t.Fatalf("pending confirmation = %+v, want mail.send", result.Pending)
	}
	if result.Text != "Send the email to the doctor?" {
		t.Errorf("reply = %q, want the registry summary question", result.Text)
	}
	if fake.requestCount() != 1 {
		t.Errorf("provider calls = %d, want the turn to stop after the confirm request", fake.requestCount())
	}

	// The model's call is armed in the registry, and a direct Execute cannot
	// bypass the Confirmation.
	stored, ok := registry.Pending()
	if !ok || stored.Tool != "mail.send" {
		t.Fatalf("registry pending = %+v, %v, want mail.send", stored, ok)
	}
	if _, err := registry.Execute(context.Background(), "mail.send", nil); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("direct Execute error = %v, want ErrConfirmationRequired", err)
	}
	if calls != 0 {
		t.Fatalf("bypass attempt ran the confirm handler %d times", calls)
	}
}

func TestReadCallsRunConcurrentlyAndActCallsInOrder(t *testing.T) {
	var started sync.WaitGroup
	started.Add(2)
	release := make(chan struct{})

	registry := newRegistry(t)
	addTool(t, registry, tool.Tool[struct{}, struct {
		Value string `json:"value"`
	}]{
		Name: "test.alpha", Description: "First read.", Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ struct{}) (struct {
			Value string `json:"value"`
		}, error) {
			started.Done()
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				return struct {
					Value string `json:"value"`
				}{}, context.DeadlineExceeded
			}
			return struct {
				Value string `json:"value"`
			}{Value: "alpha"}, nil
		},
	})
	addTool(t, registry, tool.Tool[struct{}, struct {
		Value string `json:"value"`
	}]{
		Name: "test.beta", Description: "Second read.", Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyNo,
		Handler: func(ctx context.Context, _ struct{}) (struct {
			Value string `json:"value"`
		}, error) {
			started.Done()
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				return struct {
					Value string `json:"value"`
				}{}, context.DeadlineExceeded
			}
			return struct {
				Value string `json:"value"`
			}{Value: "beta"}, nil
		},
	})

	var orderMu sync.Mutex
	var order []string
	for _, name := range []string{"test.first", "test.second"} {
		name := name
		addTool(t, registry, tool.Tool[struct{}, struct{}]{
			Name: name, Description: "Act.", Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo,
			Handler: func(context.Context, struct{}) (struct{}, error) {
				orderMu.Lock()
				order = append(order, name)
				orderMu.Unlock()
				return struct{}{}, nil
			},
		})
	}

	fake := newFakeProvider(t,
		toolCallReply("",
			fakeCall{ID: "1", Name: "test__alpha", Args: "{}"},
			fakeCall{ID: "2", Name: "test__beta", Args: "{}"},
			fakeCall{ID: "3", Name: "test__first", Args: "{}"},
			fakeCall{ID: "4", Name: "test__second", Args: "{}"},
		),
		textReply("Done."),
	)

	// Reads can only pass once both have started, so a sequential loop fails.
	go func() {
		started.Wait()
		close(release)
	}()

	a := newTestAgent(t, testSettings(fake.start().URL), registry, nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-5"}, "do the things", nil)
	if result.Text != "Done." {
		t.Fatalf("reply = %q, want Done.", result.Text)
	}
	if len(order) != 2 || order[0] != "test.first" || order[1] != "test.second" {
		t.Fatalf("act order = %v, want model order [test.first test.second]", order)
	}
}

func TestProviderFailuresPlayTheUnavailableMessage(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500} {
		status := status
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var logs strings.Builder
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			fake := newFakeProvider(t, textReply("unused"))
			fake.status = status
			a := newTestAgent(t, testSettings(fake.start().URL), nil, logger)

			result := a.Run(context.Background(), tool.Request{ID: "req-6"}, "hello", nil)
			if result.Text != "Sorry, I can't answer that right now." {
				t.Errorf("reply = %q, want the configured Unavailable message", result.Text)
			}
			if !result.Unavailable {
				t.Error("result.Unavailable = false")
			}
			if !strings.Contains(logs.String(), fmt.Sprintf(`"status":%d`, status)) {
				t.Errorf("log does not record status %d: %s", status, logs.String())
			}
		})
	}
}

func TestFirstTokenTimeoutIsEnforced(t *testing.T) {
	fake := newFakeProvider(t, textReply("too late"))
	fake.delay = 3 * time.Second
	settings := testSettings(fake.start().URL)
	settings.FirstTokenTimeout = 100 * time.Millisecond
	a := newTestAgent(t, settings, nil, nil)

	started := time.Now()
	result := a.Run(context.Background(), tool.Request{ID: "req-7"}, "hello", nil)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run took %v, want the first-token timeout to fire quickly", elapsed)
	}
	if !result.TimedOut {
		t.Error("result.TimedOut = false")
	}
	if result.Text != agent.FirstTokenTimeoutMessage {
		t.Errorf("reply = %q, want %q", result.Text, agent.FirstTokenTimeoutMessage)
	}
}

func TestStreamingHookReceivesDeltas(t *testing.T) {
	fake := newFakeProvider(t, textReply("Hel", "lo."))
	a := newTestAgent(t, testSettings(fake.start().URL), nil, nil)

	var deltas []string
	result := a.Run(context.Background(), tool.Request{ID: "req-8"}, "hi", func(delta string) {
		deltas = append(deltas, delta)
	})
	if result.Text != "Hello." {
		t.Fatalf("reply = %q, want Hello.", result.Text)
	}
	if strings.Join(deltas, "") != "Hello." {
		t.Fatalf("deltas = %v, want the full reply", deltas)
	}
}

func bodyString(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(raw)
}

// offersTool reports whether the outgoing request lists the given wire name.
func offersTool(t *testing.T, body map[string]any, wireName string) bool {
	t.Helper()
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := entry["function"].(map[string]any)
		if ok && fn["name"] == wireName {
			return true
		}
	}
	return false
}

// TestInvalidStreamedArgumentsNeverRunATool is the fail-closed regression for
// malformed model JSON: a truncated act call must become a Tool error, never a
// zero-value act such as "volume up" or "cancel every timer".
func TestInvalidStreamedArgumentsNeverRunATool(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"truncated JSON", `{"delta":`},
		{"wrong type", `{"delta":"loud"}`},
		{"unknown field", `{"other":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			registry := newRegistry(t)
			addTool(t, registry, tool.Tool[struct {
				Delta int `json:"delta"`
			}, struct{}]{
				Name: "test.act", Description: "Change something.", Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo,
				Handler: func(context.Context, struct {
					Delta int `json:"delta"`
				}) (struct{}, error) {
					runs++
					return struct{}{}, nil
				},
			})
			fake := newFakeProvider(t,
				toolCallReply("", fakeCall{ID: "call-1", Name: "test__act", Args: tc.args}),
				textReply("I could not do that."),
			)
			a := newTestAgent(t, testSettings(fake.start().URL), registry, nil)

			result := a.Run(context.Background(), tool.Request{ID: "req-invalid"}, "do the thing", nil)
			if runs != 0 {
				t.Fatalf("act handler ran %d times, want 0", runs)
			}
			if result.Text != "I could not do that." {
				t.Fatalf("reply = %q, want the model's follow-up", result.Text)
			}
			if fake.requestCount() != 2 {
				t.Fatalf("provider calls = %d, want 2", fake.requestCount())
			}
			second := bodyString(t, fake.request(t, 1).Body)
			if !strings.Contains(second, "invalid") && !strings.Contains(second, "arguments") {
				t.Fatalf("second request carries no Tool error: %s", second)
			}
		})
	}
}

// TestBargeInCancellationIsNotAProviderFailure covers the F5/#29 defect: a
// cancelled turn used to log "agent request failed", return Unavailable and set
// agent_error on an otherwise cancelled request.
func TestBargeInCancellationIsNotAProviderFailure(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	fake := newFakeProvider(t, textReply("too late"))
	fake.delay = 2 * time.Second
	a := newTestAgent(t, testSettings(fake.start().URL), nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	result := a.Run(ctx, tool.Request{ID: "req-cancel"}, "tell me a long story", nil)

	if !result.Cancelled {
		t.Fatal("result.Cancelled = false")
	}
	if result.Unavailable || result.TimedOut {
		t.Fatalf("cancellation was classified as a provider failure: %+v", result)
	}
	if result.Text != "" {
		t.Fatalf("reply = %q, want no spoken reply on cancellation", result.Text)
	}
	if strings.Contains(logs.String(), "agent request failed") {
		t.Fatalf("cancellation was logged as a provider failure: %s", logs.String())
	}
}

// TestConfirmProposalRunsSiblingCalls covers #20's one-step contract: a confirm
// call waits for the spoken yes, but sibling reads/acts in the same model step
// still run, and exactly one pending confirmation is armed.
func TestConfirmProposalRunsSiblingCalls(t *testing.T) {
	registry := newRegistry(t)
	reads, acts, confirms := 0, 0, 0
	addTool(t, registry, tool.Tool[struct{}, struct{}]{
		Name: "test.read", Description: "Read.", Permission: tool.PermissionRead, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (struct{}, error) { reads++; return struct{}{}, nil },
	})
	addTool(t, registry, tool.Tool[struct{}, struct{}]{
		Name: "test.act", Description: "Act.", Permission: tool.PermissionAct, LocalOnly: tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (struct{}, error) { acts++; return struct{}{}, nil },
	})
	addTool(t, registry, tool.Tool[struct{}, struct{}]{
		Name: "mail.send", Description: "Send.", Permission: tool.PermissionConfirm, LocalOnly: tool.LocalOnlyNo,
		Summary: func(struct{}) string { return "Send it?" },
		Handler: func(context.Context, struct{}) (struct{}, error) { confirms++; return struct{}{}, nil },
	})

	fake := newFakeProvider(t, toolCallReply("",
		fakeCall{ID: "1", Name: "test__read", Args: "{}"},
		fakeCall{ID: "2", Name: "test__act", Args: "{}"},
		fakeCall{ID: "3", Name: "mail__send", Args: "{}"},
	))
	a := newTestAgent(t, testSettings(fake.start().URL), registry, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-confirm"}, "read, act, then send", nil)
	if reads != 1 || acts != 1 {
		t.Fatalf("sibling calls = read:%d act:%d, want 1 each", reads, acts)
	}
	if confirms != 0 {
		t.Fatalf("confirm handler ran %d times before the spoken yes", confirms)
	}
	if result.Pending == nil || result.Pending.Tool != "mail.send" || result.Text != "Send it?" {
		t.Fatalf("result = %+v, want the mail.send question", result)
	}
	if fake.requestCount() != 1 {
		t.Fatalf("provider calls = %d, want the turn to stop after the confirm", fake.requestCount())
	}
}

func TestUnavailableMessageFuncAppliesToTheNextFailure(t *testing.T) {
	fake := newFakeProvider(t, textReply("unused"))
	fake.status = 500
	settings := testSettings(fake.start().URL)
	current := "First unavailable message."
	settings.UnavailableMessageFunc = func() string { return current }
	a := newTestAgent(t, settings, nil, nil)

	result := a.Run(context.Background(), tool.Request{ID: "req-live-1"}, "hello", nil)
	if result.Text != "First unavailable message." {
		t.Fatalf("first failure = %q", result.Text)
	}
	if !result.Unavailable {
		t.Error("first result.Unavailable = false")
	}

	current = "Second unavailable message."
	result = a.Run(context.Background(), tool.Request{ID: "req-live-2"}, "hello", nil)
	if result.Text != "Second unavailable message." {
		t.Fatalf("second failure = %q, want the changed wording", result.Text)
	}

	// An empty resolver result falls back to the required static value rather
	// than speaking nothing.
	current = " "
	result = a.Run(context.Background(), tool.Request{ID: "req-live-3"}, "hello", nil)
	if result.Text != settings.UnavailableMessage {
		t.Fatalf("empty resolver result = %q, want the static fallback", result.Text)
	}
}

// openSecretService gives the Agent path the same real store-backed resolver
// production uses, so key precedence is exercised at the provider boundary.
func openSecretService(t *testing.T, names ...string) *secrets.Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "assistant.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	svc, err := secrets.New(context.Background(), st.DB(), names)
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	return svc
}

func TestAgentKeyIsResolvedPerRequestAndAStoredValueTakesOverWithoutRestart(t *testing.T) {
	const keyName = "AGENT_TEST_SECRET_KEY"
	t.Setenv(keyName, "env-key-123456")
	svc := openSecretService(t, keyName)
	fake := newFakeProvider(t, textReply("First."), textReply("Second."))
	settings := testSettings(fake.start().URL)
	settings.APIKeyEnv = keyName
	settings.APIKey = func() string {
		value, _ := svc.Get(keyName)
		return value
	}
	a, err := agent.New(settings, newRegistry(t), nil)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	if result := a.Run(context.Background(), tool.Request{ID: "req-env"}, "hello", nil); result.Text != "First." {
		t.Fatalf("first reply = %q, want First.", result.Text)
	}
	if got := fake.request(t, 0).Header.Get("Authorization"); got != "Bearer env-key-123456" {
		t.Fatalf("first Authorization = %q, want the environment key", got)
	}

	if _, err := svc.Set(context.Background(), keyName, "stored-key-abcdef"); err != nil {
		t.Fatalf("secrets.Set: %v", err)
	}
	if result := a.Run(context.Background(), tool.Request{ID: "req-store"}, "hello again", nil); result.Text != "Second." {
		t.Fatalf("second reply = %q, want Second.", result.Text)
	}
	if got := fake.request(t, 1).Header.Get("Authorization"); got != "Bearer stored-key-abcdef" {
		t.Fatalf("second Authorization = %q, want the stored key without a restart", got)
	}
}

func TestAgentStartsWithNoKeyAndUsesAStoredKeyStoredAfterStartup(t *testing.T) {
	const keyName = "AGENT_TEST_STORE_ONLY_KEY"
	t.Setenv(keyName, "")
	svc := openSecretService(t, keyName)
	fake := newFakeProvider(t, textReply("Stored."))
	settings := testSettings(fake.start().URL)
	settings.APIKeyEnv = keyName
	settings.APIKey = func() string {
		value, _ := svc.Get(keyName)
		return value
	}
	// Startup sees no key at all: this must stay constructible, not disable
	// the provider for the life of the process.
	a, err := agent.New(settings, newRegistry(t), nil)
	if err != nil {
		t.Fatalf("agent.New without a key: %v", err)
	}
	if _, err := svc.Set(context.Background(), keyName, "store-only-key-abcdef"); err != nil {
		t.Fatalf("secrets.Set: %v", err)
	}
	if result := a.Run(context.Background(), tool.Request{ID: "req-store-alone"}, "hello", nil); result.Text != "Stored." {
		t.Fatalf("reply = %q, want Stored.", result.Text)
	}
	if got := fake.request(t, 0).Header.Get("Authorization"); got != "Bearer store-only-key-abcdef" {
		t.Fatalf("Authorization = %q, want the stored key with no environment value", got)
	}
}
