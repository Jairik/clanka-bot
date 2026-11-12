package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/web"
	"gopkg.in/yaml.v3"
)

// webModule configures the real web module against a fake SearXNG URL through
// the same tools.web YAML section production decodes.
func webModule(t *testing.T, baseURL string) *web.Module {
	t.Helper()
	module := web.NewModule()
	var node yaml.Node
	if err := node.Encode(map[string]any{
		"backend":  "searxng",
		"base_url": baseURL,
		"timeout":  "1s",
	}); err != nil {
		t.Fatalf("encode tools.web: %v", err)
	}
	if err := module.Configure(&node); err != nil {
		t.Fatalf("configure tools.web: %v", err)
	}
	return module
}

func webRegistry(t *testing.T, module *web.Module) *tool.Registry {
	t.Helper()
	register := newRegistry(t)
	if err := module.Register(register, tool.Deps{}); err != nil {
		t.Fatalf("register web: %v", err)
	}
	return register
}

// TestAgentAnswersWhoWonTheMatchWithWebSearch is the #66 end-to-end Agent
// journey: the real Agent loop, real web.search Tool, real HTTP keep-alive
// client against a fake SearXNG, and a scripted provider answer. Only the
// model and SearXNG are fake; the Tool contract, prompt and loop are real.
func TestAgentAnswersWhoWonTheMatchWithWebSearch(t *testing.T) {
	var gotQuery string
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("SearXNG path = %q, want /search", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("q")
		if format := r.URL.Query().Get("format"); format != "json" {
			t.Errorf("format = %q, want json", format)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"City beat United 2-1","url":"https://news.example.test/report","content":"A stoppage-time winner settled last night's match."},
			{"title":"Match report","url":"https://sport.example.test/report","content":"City won 2-1 at home."}
		]}`)
	}))
	defer search.Close()

	fake := newFakeProvider(t,
		toolCallReply("",
			fakeCall{ID: "call-search", Name: "web__search", Args: `{"query":"who won the match last night","max_results":5}`}),
		textReply("City beat United 2-1 last night."),
	)
	server := fake.start()

	a := newTestAgent(t, testSettings(server.URL), webRegistry(t, webModule(t, search.URL)), nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-web-match", Device: tool.DevicePi},
		"who won the match last night", nil)

	if gotQuery != "who won the match last night" {
		t.Fatalf("SearXNG query = %q", gotQuery)
	}
	if result.Text != "City beat United 2-1 last night." {
		t.Fatalf("reply = %q", result.Text)
	}
	if len(result.Calls) != 1 || result.Calls[0].Name != web.Name {
		t.Fatalf("calls = %+v, want one web.search call", result.Calls)
	}
	if strings.Contains(string(result.Calls[0].Result), "error") {
		t.Fatalf("web.search failed: %s", result.Calls[0].Result)
	}

	// The model was offered the Tool under its wire name, with query required.
	first := fake.request(t, 0)
	firstTools, err := json.Marshal(first.Body["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if !strings.Contains(string(firstTools), `"web__search"`) {
		t.Fatalf("the model was not offered web.search: %s", firstTools)
	}
	if !strings.Contains(string(firstTools), `"required":["query"]`) {
		t.Fatalf("web.search schema does not require query: %s", firstTools)
	}

	// The second provider request carries the real Tool result, so the
	// scripted answer is grounded in the search, not guessed.
	secondMessages, err := json.Marshal(fake.request(t, 1).Body["messages"])
	if err != nil {
		t.Fatalf("marshal second messages: %v", err)
	}
	if !strings.Contains(string(secondMessages), "A stoppage-time winner settled last night") {
		t.Fatalf("the Tool result did not reach the model: %s", secondMessages)
	}
	if !strings.Contains(string(secondMessages), `"role":"tool"`) {
		t.Fatalf("no tool message in the follow-up request: %s", secondMessages)
	}
}

// TestAgentSaysItCouldNotSearchWhenSearXNGIsDown proves a dead backend is a
// clean Tool failure the model can relay, not a fabricated answer.
func TestAgentSaysItCouldNotSearchWhenSearXNGIsDown(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	searchURL := search.URL
	search.Close()

	fake := newFakeProvider(t,
		toolCallReply("",
			fakeCall{ID: "call-search", Name: "web__search", Args: `{"query":"who won the match last night"}`}),
		textReply("I couldn't search the web right now."),
	)
	server := fake.start()

	a := newTestAgent(t, testSettings(server.URL), webRegistry(t, webModule(t, searchURL)), nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-web-down", Device: tool.DevicePi},
		"who won the match last night", nil)

	if result.Text != "I couldn't search the web right now." {
		t.Fatalf("reply = %q", result.Text)
	}
	if len(result.Calls) != 1 || result.Calls[0].Name != web.Name {
		t.Fatalf("calls = %+v, want one web.search call", result.Calls)
	}
	if !strings.Contains(string(result.Calls[0].Result), "I couldn't search the web right now.") {
		t.Fatalf("Tool result did not carry the speakable failure: %s", result.Calls[0].Result)
	}
	// The same honest line was reported back to the model.
	messages, err := json.Marshal(fake.request(t, 1).Body["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	if !strings.Contains(string(messages), "I couldn't search the web right now.") {
		t.Fatalf("the failure did not reach the model: %s", messages)
	}
}
