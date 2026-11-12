package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/github"
	"gopkg.in/yaml.v3"
)

// githubJourneySecrets supplies the module's two declared tokens to the real
// resolver seam; the fake GitHub never sees a real credential.
type githubJourneySecrets struct{}

func (githubJourneySecrets) Get(name string) (string, bool) {
	if name == github.SecretName {
		return "github_pat_agent_journey_token", true
	}
	if name == github.NotificationsSecretName {
		return "ghp_agent_journey_classic_token", true
	}
	return "", false
}

// githubReadRegistry wires the real github module against the fake API.
func githubReadRegistry(t *testing.T, baseURL string) *tool.Registry {
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
	registry := newRegistry(t)
	if err := module.Register(registry, tool.Deps{Secrets: githubJourneySecrets{}}); err != nil {
		t.Fatalf("register github: %v", err)
	}
	return registry
}

// TestAgentAnswersPRsWaitingAndNotifications is the read-side Agent journey:
// the real Agent loop calls both Agent-only GitHub Tools against a fake API,
// and the model's follow-up request carries their bounded results. The model is
// a scripted SSE provider; only the model and GitHub are fake.
func TestAgentAnswersPRsWaitingAndNotifications(t *testing.T) {
	var sawMu sync.Mutex
	var sawSearch, sawNotifications bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/issues":
			sawMu.Lock()
			sawSearch = true
			sawMu.Unlock()
			if got := r.URL.Query().Get("q"); got != "is:open is:pr review-requested:@me" {
				t.Errorf("search q = %q, want review-requested:@me", got)
			}
			fmt.Fprint(w, `{"total_count":1,"items":[
				{"number":12,"title":"Add timer persistence","html_url":"https://github.com/acme/widgets/pull/12","repository_url":"https://api.github.com/repos/acme/widgets","updated_at":"2026-11-11T10:00:00Z","user":{"login":"alice"}}
			]}`)
		case "/notifications":
			sawMu.Lock()
			sawNotifications = true
			sawMu.Unlock()
			if got := r.URL.Query().Get("all"); got != "false" {
				t.Errorf("notifications all = %q, want false", got)
			}
			fmt.Fprint(w, `[{"reason":"review_requested","unread":true,"updated_at":"2026-11-11T10:00:00Z","subject":{"title":"Please review the timer fix","type":"PullRequest"},"repository":{"full_name":"acme/widgets"}}]`)
		default:
			t.Errorf("unexpected GitHub path %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	fake := newFakeProvider(t,
		toolCallReply("",
			fakeCall{ID: "call-prs", Name: "github__pr_list", Args: `{}`},
			fakeCall{ID: "call-notifications", Name: "github__notifications", Args: `{}`},
		),
		textReply("You have one PR waiting for review and one unread notification."),
	)
	server := fake.start()

	a := newTestAgent(t, testSettings(server.URL), githubReadRegistry(t, api.URL), nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-github-read", Device: tool.DevicePi},
		"any PRs waiting on me and what are my GitHub notifications", nil)

	if result.Text != "You have one PR waiting for review and one unread notification." {
		t.Fatalf("reply = %q", result.Text)
	}
	sawMu.Lock()
	searched, notified := sawSearch, sawNotifications
	sawMu.Unlock()
	if !searched || !notified {
		t.Fatalf("GitHub requests: search=%v notifications=%v, want both", searched, notified)
	}
	if len(result.Calls) != 2 {
		t.Fatalf("calls = %+v, want both read Tools", result.Calls)
	}
	if result.Calls[0].Name != github.PRListName || result.Calls[1].Name != github.NotificationsName {
		t.Fatalf("calls = %+v, want pr_list then notifications", result.Calls)
	}

	// The model was offered both Tools with the untrusted-facts label.
	firstTools, err := json.Marshal(fake.request(t, 0).Body["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, want := range []string{"github__pr_list", "github__notifications", "untrusted"} {
		if !strings.Contains(string(firstTools), want) {
			t.Errorf("model tools missing %q: %s", want, firstTools)
		}
	}

	// The follow-up request carries both real Tool results, so the answer is
	// grounded in GitHub rather than guessed.
	secondMessages, err := json.Marshal(fake.request(t, 1).Body["messages"])
	if err != nil {
		t.Fatalf("marshal second messages: %v", err)
	}
	for _, want := range []string{"Add timer persistence", "Please review the timer fix", `"role":"tool"`} {
		if !strings.Contains(string(secondMessages), want) {
			t.Errorf("follow-up messages missing %q: %s", want, secondMessages)
		}
	}
}
