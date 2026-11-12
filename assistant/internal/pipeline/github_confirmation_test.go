package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/github"
	"gopkg.in/yaml.v3"
)

// ghJourneySecrets is the one declared GitHub token for the journey tests.
type ghJourneySecrets struct{ token string }

func (s ghJourneySecrets) Get(name string) (string, bool) {
	if name != github.SecretName || s.token == "" {
		return "", false
	}
	return s.token, true
}

// ghJourneyGitHub is the scripted GitHub API. It records only the create calls
// the journey must not make before a clear yes.
type ghJourneyGitHub struct {
	mu      sync.Mutex
	creates int
}

func (g *ghJourneyGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/widgets/issues" {
			t.Errorf("GitHub request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		g.mu.Lock()
		g.creates++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number":68,"title":"Timers don't survive restart","html_url":"https://github.com/acme/widgets/issues/68"}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func (g *ghJourneyGitHub) createCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.creates
}

// ghJourneyModel is a scripted OpenAI-compatible SSE endpoint. It is a fake
// model, not a fake semantic router: the real Agent loop and Tool registry run.
type ghJourneyModel struct {
	mu       sync.Mutex
	replies  []string
	requests []map[string]any
}

func (m *ghJourneyModel) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode model request: %v", err)
		}
		m.mu.Lock()
		m.requests = append(m.requests, body)
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

func (m *ghJourneyModel) request(t *testing.T, index int) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if index >= len(m.requests) {
		t.Fatalf("model request %d missing (got %d)", index, len(m.requests))
	}
	return m.requests[index]
}

func (m *ghJourneyModel) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// ghStreamedText renders a plain streamed assistant answer.
func ghStreamedText(text string) string {
	chunk := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	finish := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}},
	}
	finishRaw, _ := json.Marshal(finish)
	return "data: " + string(raw) + "\n\ndata: " + string(finishRaw) + "\n\ndata: [DONE]\n\n"
}

// ghJourneyConfig builds the tools.github section against the fake API.
func ghJourneyConfig(t *testing.T, baseURL string) *github.Module {
	t.Helper()
	module := github.NewModule()
	var node yaml.Node
	if err := node.Encode(map[string]any{
		"base_url":      baseURL,
		"timeout":       "1s",
		"default_repo":  "acme/widgets",
		"allowed_repos": []string{"acme/widgets"},
		"results":       5,
		"max_results":   10,
	}); err != nil {
		t.Fatalf("encode tools.github: %v", err)
	}
	if err := module.Configure(&node); err != nil {
		t.Fatalf("configure tools.github: %v", err)
	}
	return module
}

// ghJourney builds the real Agent loop, the real GitHub Tools and the real
// one-turn Confirmation stage. Only the model and GitHub are fake.
func ghJourney(t *testing.T, model *ghJourneyModel, githubAPI *ghJourneyGitHub) (*pipeline.Pipeline, *tool.Registry) {
	t.Helper()
	githubServer := githubAPI.start(t)
	modelServer := model.start(t)

	reg := tool.NewRegistry(5 * time.Second)
	if err := ghJourneyConfig(t, githubServer.URL).Register(reg, tool.Deps{
		Secrets: ghJourneySecrets{token: "ghp_journey_token"},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("register github: %v", err)
	}
	t.Setenv(confirmationTestKeyEnv, "test-key")
	agentPath, err := agent.New(agent.Settings{
		BaseURL:            modelServer.URL,
		APIKeyEnv:          confirmationTestKeyEnv,
		Model:              "test-model",
		UnavailableMessage: "unavailable",
		FirstTokenTimeout:  2 * time.Second,
	}, reg, nil)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(agentPath, nil),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p, reg
}

// TestGitHubIssueCreateAsksThenCreatesOnce is the scripted-Agent journey: the
// model asks for github.issue_create, the user hears the one-line summary, no
// HTTP write happens, and a clear yes creates exactly one issue.
func TestGitHubIssueCreateAsksThenCreatesOnce(t *testing.T) {
	model := &ghJourneyModel{replies: []string{
		confirmToolCallReply("github__issue_create", `{"title":"Timers don't survive restart","body":"The timer list is empty after restart."}`),
	}}
	githubAPI := &ghJourneyGitHub{}
	p, reg := ghJourney(t, model, githubAPI)

	question := p.Handle(context.Background(), request(), "open an issue on clanka-bot: timers don't survive restart")
	if !strings.Contains(question.Text, "acme/widgets") || !strings.Contains(question.Text, "Timers don't survive restart") {
		t.Fatalf("question = %q, want the repo and title", question.Text)
	}
	if githubAPI.createCount() != 0 {
		t.Fatal("the issue was created before a clear yes")
	}
	pending, ok := reg.Pending()
	if !ok || pending.Tool != github.Name {
		t.Fatalf("pending = %+v, %v", pending, ok)
	}

	// The model was offered the Tool under its wire name, with the untrusted
	// facts label and the operator default repo in the description.
	toolsJSON, err := json.Marshal(model.request(t, 0)["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if !strings.Contains(string(toolsJSON), `"github__issue_create"`) {
		t.Fatalf("the model was not offered github.issue_create: %s", toolsJSON)
	}
	if !strings.Contains(string(toolsJSON), "untrusted") {
		t.Fatalf("the Tool output was not labelled untrusted: %s", toolsJSON)
	}
	if !strings.Contains(string(toolsJSON), "acme/widgets") {
		t.Fatalf("the Tool description does not name the default repo: %s", toolsJSON)
	}

	reply := p.Handle(context.Background(), request(), "yes")
	if reply.Text != "Opened issue #68 in acme/widgets." {
		t.Fatalf("confirmed reply = %q", reply.Text)
	}
	if reply.Path != tool.PathConfirmation {
		t.Fatalf("confirmed path = %s, want %s", reply.Path, tool.PathConfirmation)
	}
	if githubAPI.createCount() != 1 {
		t.Fatalf("create count = %d, want 1", githubAPI.createCount())
	}
	if model.requestCount() != 1 {
		t.Fatalf("model requests = %d, want the yes to need no model call", model.requestCount())
	}
	if _, ok := reg.Pending(); ok {
		t.Fatal("the pending call survived the yes")
	}
	if _, _, err := reg.ExecutePending(context.Background()); !errors.Is(err, tool.ErrNoPending) {
		t.Fatalf("a second yes error = %v, want ErrNoPending", err)
	}
	if githubAPI.createCount() != 1 {
		t.Fatalf("create count after a repeated yes = %d, want 1", githubAPI.createCount())
	}
}

// TestGitHubPendingDropsOnNoUnknownAndBargeIn covers the three ways a pending
// confirm must disappear without a write: an explicit no, an unrelated request,
// and a Barge-in drop.
func TestGitHubPendingDropsOnNoUnknownAndBargeIn(t *testing.T) {
	for _, tt := range []struct {
		name       string
		transcript string
	}{
		{name: "no", transcript: "no"},
		{name: "unknown", transcript: "what time is it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := &ghJourneyModel{replies: []string{
				confirmToolCallReply("github__issue_create", `{"title":"Timers don't survive restart"}`),
				ghStreamedText("Okay."),
			}}
			githubAPI := &ghJourneyGitHub{}
			p, reg := ghJourney(t, model, githubAPI)

			if got := p.Handle(context.Background(), request(), "open an issue about the timer"); !strings.Contains(got.Text, "acme/widgets") {
				t.Fatalf("question = %q", got.Text)
			}
			reply := p.Handle(context.Background(), request(), tt.transcript)
			if !strings.Contains(reply.Text, pipeline.CancelledReply) {
				t.Fatalf("reply = %q, want the cancelled prefix", reply.Text)
			}
			if githubAPI.createCount() != 0 {
				t.Fatalf("create count = %d, want 0 after %q", githubAPI.createCount(), tt.transcript)
			}
			if _, ok := reg.Pending(); ok {
				t.Fatal("pending survived the drop")
			}
			if _, _, err := reg.ExecutePending(context.Background()); !errors.Is(err, tool.ErrNoPending) {
				t.Fatalf("ExecutePending = %v, want ErrNoPending", err)
			}
		})
	}

	t.Run("barge-in", func(t *testing.T) {
		model := &ghJourneyModel{replies: []string{
			confirmToolCallReply("github__issue_create", `{"title":"Timers don't survive restart"}`),
			ghStreamedText("Okay."),
		}}
		githubAPI := &ghJourneyGitHub{}
		p, reg := ghJourney(t, model, githubAPI)
		_ = p.Handle(context.Background(), request(), "open an issue about the timer")
		if pending, ok := p.DropPending(); !ok || pending.Tool != github.Name {
			t.Fatalf("DropPending = %+v, %v", pending, ok)
		}
		// A stale yes after Barge-in has nothing to run and writes nothing.
		_ = p.Handle(context.Background(), request(), "yes")
		if githubAPI.createCount() != 0 {
			t.Fatalf("create count = %d, want 0", githubAPI.createCount())
		}
		if _, _, err := reg.ExecutePending(context.Background()); !errors.Is(err, tool.ErrNoPending) {
			t.Fatalf("ExecutePending = %v, want ErrNoPending", err)
		}
	})
}

// TestGitHubInvalidArgsNeverArmOrWrite covers a model call whose typed args do
// not satisfy the Tool schema: the registry refuses the proposal, the Agent
// reports it back to the model, and no GitHub write happens.
func TestGitHubInvalidArgsNeverArmOrWrite(t *testing.T) {
	model := &ghJourneyModel{replies: []string{
		confirmToolCallReply("github__issue_create", `{"repo":"acme/widgets"}`),
		ghStreamedText("I need a title for the issue."),
	}}
	githubAPI := &ghJourneyGitHub{}
	p, reg := ghJourney(t, model, githubAPI)

	reply := p.Handle(context.Background(), request(), "open an issue about the timer")
	if reply.Text != "I need a title for the issue." {
		t.Fatalf("reply = %q, want the model's honest follow-up", reply.Text)
	}
	if githubAPI.createCount() != 0 {
		t.Fatalf("create count = %d, want 0", githubAPI.createCount())
	}
	if _, ok := reg.Pending(); ok {
		t.Fatal("a schema-invalid call armed a pending confirmation")
	}
}
