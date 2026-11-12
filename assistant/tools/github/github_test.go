package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

// secretFunc adapts a test closure to tool.SecretResolver.
type secretFunc func(string) (string, bool)

func (f secretFunc) Get(name string) (string, bool) { return f(name) }

func fixedSecret(value string) secretFunc {
	return func(string) (string, bool) { return value, value != "" }
}

// logBuffer collects module logs for a privacy scan.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// requestRecord is one request the fake GitHub saw.
type requestRecord struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// fakeGitHub is a scripted GitHub API. It records every request so a test can
// prove a write was not attempted before a Confirmation, or retried after a
// failure.
type fakeGitHub struct {
	handler func(w http.ResponseWriter, r *http.Request, body []byte)

	mu       sync.Mutex
	requests []requestRecord
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, requestRecord{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Header: r.Header.Clone(),
			Body:   body,
		})
		f.mu.Unlock()
		if f.handler == nil {
			t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		f.handler(w, r, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *fakeGitHub) snapshot() []requestRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]requestRecord(nil), f.requests...)
}

func (f *fakeGitHub) count(method, path string) int {
	count := 0
	for _, req := range f.snapshot() {
		if req.Method == method && req.Path == path {
			count++
		}
	}
	return count
}

// testConfig encodes the tools.github section the way production decodes it.
func testConfig(t *testing.T, overrides map[string]any) *Module {
	t.Helper()
	values := map[string]any{
		"base_url":      "https://api.github.test",
		"timeout":       "1s",
		"default_repo":  "acme/widgets",
		"allowed_repos": []string{"acme/widgets"},
		"results":       5,
		"max_results":   10,
	}
	for key, value := range overrides {
		values[key] = value
	}
	module := NewModule()
	var node yaml.Node
	if err := node.Encode(values); err != nil {
		t.Fatalf("encode tools.github: %v", err)
	}
	if err := module.Configure(&node); err != nil {
		t.Fatalf("configure tools.github: %v", err)
	}
	return module
}

// registerModule wires the module and returns the registry and captured logs.
func registerModule(t *testing.T, module *Module, secrets tool.SecretResolver) (*tool.Registry, *logBuffer) {
	t.Helper()
	reg := tool.NewRegistry(5 * time.Second)
	logs := &logBuffer{}
	if err := module.Register(reg, tool.Deps{
		Secrets: secrets,
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	}); err != nil {
		t.Fatalf("register github: %v", err)
	}
	return reg, logs
}

// armIssue proposes one issue write and returns the pending call.
func armIssue(t *testing.T, reg *tool.Registry, args string) tool.PendingConfirmation {
	t.Helper()
	pending, err := reg.Propose(Name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return pending
}

func TestIssueCreateAsksBeforeItPostsAndPostsExactlyOnce(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number":68,"title":"Timers don't survive restart","html_url":"https://github.com/acme/widgets/issues/68"}`)
	}}
	server := fake.start(t)
	module := testConfig(t, map[string]any{"base_url": server.URL})
	reg, logs := registerModule(t, module, fixedSecret("ghp_test_token_value"))

	args := `{"title":"Timers don't survive restart","body":"The timer list is empty after restart."}`
	if _, err := reg.Execute(context.Background(), Name, json.RawMessage(args)); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("Execute error = %v, want ErrConfirmationRequired", err)
	}
	if fake.count(http.MethodPost, "/repos/acme/widgets/issues") != 0 {
		t.Fatal("the confirm Tool posted before a Confirmation")
	}
	if _, ok := reg.Pending(); ok {
		t.Fatal("Execute armed a pending call; only Propose may do that")
	}

	pending := armIssue(t, reg, args)
	if pending.Tool != Name {
		t.Fatalf("pending tool = %q, want %q", pending.Tool, Name)
	}
	if !strings.Contains(pending.Summary, "acme/widgets") || !strings.Contains(pending.Summary, "Timers don't survive restart") {
		t.Fatalf("summary = %q, want the repo and title", pending.Summary)
	}
	if strings.Contains(pending.Summary, "The timer list is empty after restart.") {
		t.Fatalf("summary = %q, want no issue body", pending.Summary)
	}

	result, done, err := reg.ExecutePending(context.Background())
	if err != nil {
		t.Fatalf("ExecutePending: %v", err)
	}
	if done.Tool != Name {
		t.Fatalf("confirmed tool = %q", done.Tool)
	}
	out, ok := result.(IssueCreateOut)
	if !ok {
		t.Fatalf("result type = %T", result)
	}
	if out.Repo != "acme/widgets" || out.Number != 68 || out.URL != "https://github.com/acme/widgets/issues/68" {
		t.Fatalf("result = %+v", out)
	}
	if got := fake.count(http.MethodPost, "/repos/acme/widgets/issues"); got != 1 {
		t.Fatalf("POST count = %d, want exactly 1", got)
	}

	// The request carries the documented API version, the bearer token and a
	// User-Agent, and the body has the typed arguments.
	requests := fake.snapshot()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	req := requests[0]
	if got := req.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
		t.Fatalf("X-GitHub-Api-Version = %q, want %q", got, apiVersion)
	}
	if got := req.Header.Get("Accept"); got != acceptJSON {
		t.Fatalf("Accept = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ghp_test_token_value" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("User-Agent"); !strings.Contains(got, "clanka-bot") {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode issue body: %v", err)
	}
	if body.Title != "Timers don't survive restart" || body.Body != "The timer list is empty after restart." {
		t.Fatalf("issue body = %+v", body)
	}

	// A second yes has nothing to run and cannot duplicate the issue.
	if _, _, err := reg.ExecutePending(context.Background()); !errors.Is(err, tool.ErrNoPending) {
		t.Fatalf("second ExecutePending error = %v, want ErrNoPending", err)
	}
	if got := fake.count(http.MethodPost, "/repos/acme/widgets/issues"); got != 1 {
		t.Fatalf("POST count after a repeated yes = %d, want 1", got)
	}

	// The token, title and body are not in the local logs.
	logged := logs.String()
	for _, secret := range []string{"ghp_test_token_value", "Timers don't survive restart", "The timer list is empty after restart."} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log contains private value %q: %s", secret, logged)
		}
	}
}

func TestConfirmedArgsAreImmutableAndTyped(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number":1,"title":"Original title","html_url":"https://github.com/acme/widgets/issues/1"}`)
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	raw := []byte(`{"title":"Original title"}`)
	if _, err := reg.Propose(Name, raw); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	// Mutate the caller's backing array after the proposal. The registry must
	// hold its own copy, and the confirmed operation is the typed In value.
	raw[10] = 'X'
	if _, _, err := reg.ExecutePending(context.Background()); err != nil {
		t.Fatalf("ExecutePending: %v", err)
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(fake.snapshot()[0].Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Title != "Original title" {
		t.Fatalf("confirmed title = %q, want the immutable original", body.Title)
	}
}

func TestIssueCreateRefusesOutsideTargetsBeforeAnyHTTP(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		t.Error("an outside target reached GitHub")
		http.Error(w, "no", http.StatusInternalServerError)
	}}
	server := fake.start(t)

	tests := []struct {
		name   string
		config map[string]any
		args   string
		want   string
	}{
		{
			name: "repo outside allowlist",
			args: `{"repo":"evil/other","title":"Write elsewhere"}`,
			want: "not allowed",
		},
		{
			name: "bare repo name",
			args: `{"repo":"widgets","title":"Write somewhere"}`,
			want: "owner/name",
		},
		{
			name:   "no default and no repo",
			config: map[string]any{"default_repo": "", "allowed_repos": []string{}},
			args:   `{"title":"No target"}`,
			want:   "No GitHub repository is configured",
		},
		{
			name:   "empty allowlist refuses a supplied repo",
			config: map[string]any{"default_repo": "", "allowed_repos": []string{}},
			args:   `{"repo":"acme/widgets","title":"No target"}`,
			want:   "not allowed",
		},
		{
			name: "empty title",
			args: `{"title":"   "}`,
			want: "What should the issue be called",
		},
		{
			name: "title too long",
			args: fmt.Sprintf(`{"title":%q}`, strings.Repeat("t", maxTitleRunes+1)),
			want: "title is too long",
		},
		{
			name: "body too long",
			args: fmt.Sprintf(`{"title":"ok","body":%q}`, strings.Repeat("b", maxBodyRunes+1)),
			want: "body is too long",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overrides := map[string]any{"base_url": server.URL}
			for key, value := range tt.config {
				overrides[key] = value
			}
			reg, _ := registerModule(t, testConfig(t, overrides), fixedSecret("token"))
			armIssue(t, reg, tt.args)
			_, _, err := reg.ExecutePending(context.Background())
			if err == nil {
				t.Fatal("outside target succeeded")
			}
			if got := tool.FailureMessage(err); !strings.Contains(got, tt.want) {
				t.Fatalf("failure = %q, want it to contain %q", got, tt.want)
			}
		})
	}
	if got := len(fake.snapshot()); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}

	// A missing required title is rejected by the schema before a pending call
	// exists, so a malformed model call cannot be confirmed later.
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
	if _, err := reg.Propose(Name, json.RawMessage(`{"repo":"acme/widgets"}`)); err == nil {
		t.Fatal("Propose accepted a missing title")
	}
	if _, ok := reg.Pending(); ok {
		t.Fatal("a schema-invalid proposal armed a pending call")
	}
}

func TestIssueCreateDoesNotRetryOnFailure(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}}
		server := fake.start(t)
		reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
		armIssue(t, reg, `{"title":"Will fail"}`)
		_, _, err := reg.ExecutePending(context.Background())
		if err == nil {
			t.Fatal("500 succeeded")
		}
		if got := tool.FailureMessage(err); !strings.Contains(got, "didn't retry") {
			t.Fatalf("failure = %q, want the no-retry line", got)
		}
		if got := fake.count(http.MethodPost, "/repos/acme/widgets/issues"); got != 1 {
			t.Fatalf("POST count = %d, want exactly 1", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(http.StatusCreated)
		}}
		server := fake.start(t)
		reg, _ := registerModule(t, testConfig(t, map[string]any{
			"base_url": server.URL,
			"timeout":  "50ms",
		}), fixedSecret("token"))
		armIssue(t, reg, `{"title":"Will time out"}`)
		_, _, err := reg.ExecutePending(context.Background())
		if err == nil {
			t.Fatal("timeout succeeded")
		}
		if got := tool.FailureMessage(err); !strings.Contains(got, "didn't retry") {
			t.Fatalf("failure = %q, want the no-retry line", got)
		}
		if got := fake.count(http.MethodPost, "/repos/acme/widgets/issues"); got != 1 {
			t.Fatalf("POST count = %d, want exactly 1", got)
		}
	})

	t.Run("auth failure", func(t *testing.T) {
		fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
		}}
		server := fake.start(t)
		reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
		armIssue(t, reg, `{"title":"Will fail"}`)
		_, _, err := reg.ExecutePending(context.Background())
		if got := tool.FailureMessage(err); !strings.Contains(got, "rejected the token") {
			t.Fatalf("failure = %q", got)
		}
	})
}

func TestPRListUsesReviewRequestedNotAllOpenPullRequests(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":2,"items":[
			{"number":12,"title":"Add timer persistence","html_url":"https://github.com/acme/widgets/pull/12","repository_url":"https://api.github.com/repos/acme/widgets","updated_at":"2026-11-11T10:00:00Z","draft":false,"user":{"login":"alice"}},
			{"number":13,"title":"Fix restart bug","html_url":"https://github.com/acme/widgets/pull/13","repository_url":"https://api.github.com/repos/acme/widgets","updated_at":"2026-11-11T11:00:00Z","draft":true,"user":{"login":"bob"}}
		]}`)
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	result, err := reg.Execute(context.Background(), PRListName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(PRListOut)
	if out.Total != 2 || out.Returned != 2 || out.Truncated {
		t.Fatalf("out counts = %+v", out)
	}
	if len(out.PullRequests) != 2 {
		t.Fatalf("pull requests = %+v", out.PullRequests)
	}
	first := out.PullRequests[0]
	if first.Repo != "acme/widgets" || first.Number != 12 || first.Title != "Add timer persistence" ||
		first.URL != "https://github.com/acme/widgets/pull/12" || first.Author != "alice" || first.Draft {
		t.Fatalf("first PR = %+v", first)
	}
	if !out.PullRequests[1].Draft {
		t.Fatal("draft flag was lost")
	}

	requests := fake.snapshot()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	req := requests[0]
	if req.Path != "/search/issues" {
		t.Fatalf("path = %q", req.Path)
	}
	if got := req.Query.Get("q"); got != "is:open is:pr review-requested:@me" {
		t.Fatalf("q = %q, want review-requested:@me", got)
	}
	if got := req.Query.Get("per_page"); got != "10" {
		t.Fatalf("per_page = %q", got)
	}
	if got := req.Query.Get("sort"); got != "updated" {
		t.Fatalf("sort = %q", got)
	}
	if got := req.Query.Get("order"); got != "desc" {
		t.Fatalf("order = %q", got)
	}
	if got := req.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
		t.Fatalf("X-GitHub-Api-Version = %q", got)
	}
	// "waiting on me" uses @me, so no identity request is needed.
	for _, recorded := range requests {
		if recorded.Path == "/user" {
			t.Fatal("pr_list made an unnecessary identity request")
		}
	}

	// An explicit repo narrows the same query and must be an allowed selector.
	if _, err := reg.Execute(context.Background(), PRListName, json.RawMessage(`{"repo":"acme/widgets"}`)); err != nil {
		t.Fatalf("repo filter: %v", err)
	}
	requests = fake.snapshot()
	if got := requests[len(requests)-1].Query.Get("q"); got != "is:open is:pr review-requested:@me repo:acme/widgets" {
		t.Fatalf("filtered q = %q", got)
	}
	if _, err := reg.Execute(context.Background(), PRListName, json.RawMessage(`{"repo":"evil/other"}`)); err == nil {
		t.Fatal("an outside repo filter succeeded")
	}
	if got := len(fake.snapshot()); got != 2 {
		t.Fatalf("requests after the refused filter = %d, want 2", got)
	}
}

func TestPRListPaginatesWithinItsBoundAndCounts(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			// A next link is followed only because the caller asked for 15.
			w.Header().Set("Link", `<https://api.github.test/search/issues?page=2>; rel="next"`)
			fmt.Fprint(w, `{"total_count":25,"items":[
				{"number":1,"title":"One","html_url":"https://github.com/acme/widgets/pull/1","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":2,"title":"Two","html_url":"https://github.com/acme/widgets/pull/2","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":3,"title":"Three","html_url":"https://github.com/acme/widgets/pull/3","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":4,"title":"Four","html_url":"https://github.com/acme/widgets/pull/4","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":5,"title":"Five","html_url":"https://github.com/acme/widgets/pull/5","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":6,"title":"Six","html_url":"https://github.com/acme/widgets/pull/6","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":7,"title":"Seven","html_url":"https://github.com/acme/widgets/pull/7","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":8,"title":"Eight","html_url":"https://github.com/acme/widgets/pull/8","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":9,"title":"Nine","html_url":"https://github.com/acme/widgets/pull/9","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":10,"title":"Ten","html_url":"https://github.com/acme/widgets/pull/10","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}}
			]}`)
		default:
			fmt.Fprint(w, `{"total_count":25,"items":[
				{"number":11,"title":"Eleven","html_url":"https://github.com/acme/widgets/pull/11","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":12,"title":"Twelve","html_url":"https://github.com/acme/widgets/pull/12","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":13,"title":"Thirteen","html_url":"https://github.com/acme/widgets/pull/13","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":14,"title":"Fourteen","html_url":"https://github.com/acme/widgets/pull/14","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}},
				{"number":15,"title":"Fifteen","html_url":"https://github.com/acme/widgets/pull/15","repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":"a"}}
			]}`)
		}
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{
		"base_url":    server.URL,
		"results":     15,
		"max_results": 20,
	}), fixedSecret("token"))

	result, err := reg.Execute(context.Background(), PRListName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(PRListOut)
	if out.Total != 25 || out.Returned != 15 || !out.Truncated {
		t.Fatalf("out counts = %+v, want total 25, returned 15, truncated", out)
	}
	if got := len(fake.snapshot()); got != maxListPages {
		t.Fatalf("pages fetched = %d, want %d", got, maxListPages)
	}
}

func TestPRListBoundsTextAndReturnsAnEmptySlice(t *testing.T) {
	longTitle := strings.Repeat("x", maxTitleRunes+50) + "\nsecond line"
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"total_count":1,"items":[{"number":1,"title":%q,"html_url":%q,"repository_url":"https://api.github.com/repos/acme/widgets","user":{"login":%q}}]}`,
			longTitle, "https://github.com/acme/widgets/pull/1", strings.Repeat("a", maxAuthorRunes+20))
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
	result, err := reg.Execute(context.Background(), PRListName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	pr := result.(PRListOut).PullRequests[0]
	if strings.Contains(pr.Title, "\n") || len([]rune(pr.Title)) > maxTitleRunes+1 {
		t.Fatalf("title is not collapsed and bounded: %q", pr.Title)
	}
	if len([]rune(pr.Author)) > maxAuthorRunes+1 {
		t.Fatalf("author is not bounded: %q", pr.Author)
	}

	empty := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	}}
	emptyServer := empty.start(t)
	emptyReg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": emptyServer.URL}), fixedSecret("token"))
	result, err = emptyReg.Execute(context.Background(), PRListName, nil)
	if err != nil {
		t.Fatalf("empty Execute: %v", err)
	}
	out := result.(PRListOut)
	if out.PullRequests == nil || len(out.PullRequests) != 0 {
		t.Fatalf("empty list = %#v, want a non-nil empty slice", out.PullRequests)
	}
	if out.Truncated {
		t.Fatal("an empty result was marked truncated")
	}
}

func TestNotificationsReadsUnreadAndNamesTheClassicTokenContract(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET (this Tool never marks read)", r.Method)
		}
		if got := r.URL.Query().Get("all"); got != "false" {
			t.Errorf("all = %q, want false", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"reason":"review_requested","unread":true,"updated_at":"2026-11-11T10:00:00Z","subject":{"title":"Please review","type":"PullRequest"},"repository":{"full_name":"acme/widgets"}},
			{"reason":"mention","unread":true,"updated_at":"2026-11-11T11:00:00Z","subject":{"title":"Question","type":"Issue"},"repository":{"full_name":"acme/widgets"}}
		]`)
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	result, err := reg.Execute(context.Background(), NotificationsName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(NotificationsOut)
	if out.Count != 2 || len(out.Notifications) != 2 {
		t.Fatalf("notifications = %+v", out)
	}
	if out.Notifications[0].Repo != "acme/widgets" || out.Notifications[0].Title != "Please review" ||
		out.Notifications[0].Reason != "review_requested" || out.Notifications[0].Type != "PullRequest" {
		t.Fatalf("first notification = %+v", out.Notifications[0])
	}
	for _, req := range fake.snapshot() {
		if req.Method != http.MethodGet {
			t.Fatalf("a %s %s request was made; the Tool is read-only", req.Method, req.Path)
		}
		if req.Path != "/notifications" {
			t.Fatalf("path = %q", req.Path)
		}
		if got := req.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
			t.Fatalf("X-GitHub-Api-Version = %q", got)
		}
	}
}

func TestNotificationsReportsTheFineGrainedContractHonestly(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		header  map[string]string
		want    string
		notWant string
	}{
		{
			name:    "forbidden by a fine-grained token",
			status:  http.StatusForbidden,
			want:    "classic token",
			notWant: "fine-grained tokens support",
		},
		{
			name:   "not found",
			status: http.StatusNotFound,
			want:   "classic token",
		},
		{
			name:   "gone",
			status: http.StatusGone,
			want:   "classic token",
		},
		{
			name:   "rate limited",
			status: http.StatusForbidden,
			header: map[string]string{"X-RateLimit-Remaining": "0"},
			want:   "rate limit",
		},
		{
			name:   "bad token",
			status: http.StatusUnauthorized,
			want:   "rejected the token",
		},
		{
			name:   "server error",
			status: http.StatusInternalServerError,
			want:   "couldn't read your GitHub notifications",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				for key, value := range tt.header {
					w.Header().Set(key, value)
				}
				http.Error(w, "private GitHub response body", tt.status)
			}}
			server := fake.start(t)
			reg, logs := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
			_, err := reg.Execute(context.Background(), NotificationsName, nil)
			if err == nil {
				t.Fatal("request succeeded")
			}
			got := tool.FailureMessage(err)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("failure = %q, want %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(strings.ToLower(got), strings.ToLower(tt.notWant)) {
				t.Fatalf("failure = %q claims unsupported fine-grained support", got)
			}
			if strings.Contains(logs.String(), "private GitHub response body") {
				t.Fatal("the response body reached the log")
			}
			if strings.Contains(logs.String(), "token") && strings.Contains(logs.String(), "ghp_") {
				t.Fatal("the token reached the log")
			}
		})
	}
}

func TestNotificationsPaginatesWithinItsBoundAndHandlesEmpty(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", `<https://api.github.test/notifications?page=2>; rel="next"`)
			var items []string
			for i := 1; i <= 10; i++ {
				items = append(items, fmt.Sprintf(`{"reason":"mention","unread":true,"subject":{"title":"Item %d","type":"Issue"},"repository":{"full_name":"acme/widgets"}}`, i))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
		default:
			fmt.Fprint(w, `[{"reason":"mention","unread":true,"subject":{"title":"Item 11","type":"Issue"},"repository":{"full_name":"acme/widgets"}}]`)
		}
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{
		"base_url":    server.URL,
		"results":     15,
		"max_results": 20,
	}), fixedSecret("token"))
	result, err := reg.Execute(context.Background(), NotificationsName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(NotificationsOut)
	if out.Count != 11 {
		t.Fatalf("count = %d, want 11 (two bounded pages)", out.Count)
	}
	if got := len(fake.snapshot()); got != maxListPages {
		t.Fatalf("pages = %d, want %d", got, maxListPages)
	}

	empty := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}}
	emptyServer := empty.start(t)
	emptyReg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": emptyServer.URL}), fixedSecret("token"))
	result, err = emptyReg.Execute(context.Background(), NotificationsName, nil)
	if err != nil {
		t.Fatalf("empty Execute: %v", err)
	}
	out = result.(NotificationsOut)
	if out.Notifications == nil || out.Count != 0 {
		t.Fatalf("empty notifications = %#v", out.Notifications)
	}
}

func TestListFailuresAreBoundedAndSafe(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header map[string]string
		body   string
		want   string
	}{
		{"rate limited", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "", "rate limit"},
		{"unauthorized", http.StatusUnauthorized, nil, "", "rejected the token"},
		{"forbidden", http.StatusForbidden, nil, "", "refused"},
		{"not found", http.StatusNotFound, nil, "", "couldn't find"},
		{"invalid JSON", http.StatusOK, nil, "not-json", "couldn't check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				for key, value := range tt.header {
					w.Header().Set(key, value)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if tt.body != "" {
					fmt.Fprint(w, tt.body)
				}
			}}
			server := fake.start(t)
			reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
			_, err := reg.Execute(context.Background(), PRListName, nil)
			if err == nil {
				t.Fatal("request succeeded")
			}
			if got := tool.FailureMessage(err); !strings.Contains(got, tt.want) {
				t.Fatalf("failure = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("oversize body", func(t *testing.T) {
		fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, strings.Repeat("x", maxBodyBytes+1))
		}}
		server := fake.start(t)
		reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
		if _, err := reg.Execute(context.Background(), PRListName, nil); err == nil {
			t.Fatal("oversize body succeeded")
		}
	})
}

func TestMissingTokenIsClearInTheToolsHealthAndSecretUI(t *testing.T) {
	module := testConfig(t, map[string]any{"base_url": "https://api.github.test"})
	reg, _ := registerModule(t, module, fixedSecret(""))

	if got := module.RequiredSecrets(); len(got) != 1 || got[0] != SecretName {
		t.Fatalf("RequiredSecrets = %v, want [%s]", got, SecretName)
	}
	if err := module.Health(context.Background()); err == nil || !strings.Contains(err.Error(), SecretName) {
		t.Fatalf("Health = %v, want a missing %s error", err, SecretName)
	}
	if _, err := reg.Execute(context.Background(), PRListName, nil); err == nil ||
		!strings.Contains(tool.FailureMessage(err), SecretName) {
		t.Fatalf("pr_list error = %v, want a missing %s line", err, SecretName)
	}
	armIssue(t, reg, `{"title":"No token"}`)
	if _, _, err := reg.ExecutePending(context.Background()); err == nil ||
		!strings.Contains(tool.FailureMessage(err), SecretName) {
		t.Fatalf("issue_create error = %v, want a missing %s line", err, SecretName)
	}

	// The Secrets page lists the same declaration; the real module list is what
	// feeds it, so the name is discoverable without a page edit.
	found := false
	for _, name := range reg.Names() {
		if name == Name || name == PRListName || name == NotificationsName {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry names = %v, want the three GitHub Tools", reg.Names())
	}
}

func TestHealthChecksTheTokenWithoutLeakingIt(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/user" {
			t.Errorf("health path = %q, want /user", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"octocat"}`)
	}}
	server := fake.start(t)
	logs := &logBuffer{}
	module := testConfig(t, map[string]any{"base_url": server.URL})
	reg := tool.NewRegistry(5 * time.Second)
	if err := module.Register(reg, tool.Deps{
		Secrets: fixedSecret("ghp_health_token"),
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := module.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if strings.Contains(logs.String(), "ghp_health_token") {
		t.Fatal("the token reached the log")
	}

	fake.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}
	if err := module.Health(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Health after 401 = %v", err)
	}
	if strings.Contains(logs.String(), "ghp_health_token") {
		t.Fatal("the token reached the log after a failure")
	}
}

func TestTokenIsResolvedPerCallAndNeverLogged(t *testing.T) {
	token := "ghp_first_token"
	var mu sync.Mutex
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	}}
	server := fake.start(t)
	module := testConfig(t, map[string]any{"base_url": server.URL})
	reg, logs := registerModule(t, module, secretFunc(func(string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return token, true
	}))
	if _, err := reg.Execute(context.Background(), PRListName, nil); err != nil {
		t.Fatalf("first list: %v", err)
	}
	mu.Lock()
	token = "ghp_second_token"
	mu.Unlock()
	if _, err := reg.Execute(context.Background(), PRListName, nil); err != nil {
		t.Fatalf("second list: %v", err)
	}
	requests := fake.snapshot()
	if got := requests[0].Header.Get("Authorization"); got != "Bearer ghp_first_token" {
		t.Fatalf("first Authorization = %q", got)
	}
	if got := requests[1].Header.Get("Authorization"); got != "Bearer ghp_second_token" {
		t.Fatalf("second Authorization = %q", got)
	}
	if strings.Contains(logs.String(), "ghp_first_token") || strings.Contains(logs.String(), "ghp_second_token") {
		t.Fatal("a token reached the log")
	}
}

func TestPRListReusesOneKeepAliveConnection(t *testing.T) {
	var mu sync.Mutex
	var peers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		peers = append(peers, r.RemoteAddr)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	}))
	t.Cleanup(server.Close)

	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))
	for i := 0; i < 2; i++ {
		if _, err := reg.Execute(context.Background(), PRListName, nil); err != nil {
			t.Fatalf("list %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peers) != 2 || peers[0] != peers[1] {
		t.Fatalf("remote peers = %v, want both requests on one kept-alive connection", peers)
	}
}

func TestConfigValidationFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
	}{
		{"default repo outside allowlist", map[string]any{"default_repo": "evil/other"}},
		{"default repo without a slash", map[string]any{"default_repo": "widgets"}},
		{"allowed repo with an extra slash", map[string]any{"allowed_repos": []string{"acme/widgets/extra"}}},
		{"allowed repo with an empty entry", map[string]any{"allowed_repos": []string{""}}},
		{"zero timeout", map[string]any{"timeout": "0s"}},
		{"zero results", map[string]any{"results": 0}},
		{"max below results", map[string]any{"results": 10, "max_results": 5}},
		{"max above the cap", map[string]any{"max_results": maxResultLimit + 1}},
		{"unsupported scheme", map[string]any{"base_url": "ftp://api.github.test"}},
		{"empty host", map[string]any{"base_url": "https://"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := NewModule()
			var node yaml.Node
			values := map[string]any{
				"base_url":      "https://api.github.test",
				"timeout":       "1s",
				"default_repo":  "acme/widgets",
				"allowed_repos": []string{"acme/widgets"},
				"results":       5,
				"max_results":   10,
			}
			for key, value := range tt.values {
				values[key] = value
			}
			if err := node.Encode(values); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if err := module.Configure(&node); err == nil {
				t.Fatal("Configure accepted an invalid config")
			}
		})
	}

	// Duplicate allowlist entries collapse, and surrounding whitespace is
	// trimmed, so the allowlist cannot be confused by formatting.
	module := testConfig(t, map[string]any{
		"allowed_repos": []string{" acme/widgets ", "ACME/WIDGETS"},
	})
	if got := module.cfg.AllowedRepos; len(got) != 1 || got[0] != "acme/widgets" {
		t.Fatalf("AllowedRepos = %v, want one normalized entry", got)
	}
}

func TestRepoAndBoundHelpers(t *testing.T) {
	for _, repo := range []string{"acme/widgets", "A_B/c-d.e"} {
		if owner, name, ok := splitRepo(repo); !ok || owner+"/"+name != repo {
			t.Fatalf("splitRepo(%q) = %q, %q, %v", repo, owner, name, ok)
		}
	}
	for _, repo := range []string{"", "widgets", "/widgets", "acme/", "acme/widgets/extra", "../x", "acme/.."} {
		if _, _, ok := splitRepo(repo); ok {
			t.Fatalf("splitRepo(%q) accepted an unsafe selector", repo)
		}
	}
	if got := repoFromAPIURL("https://api.github.com/repos/acme/widgets"); got != "acme/widgets" {
		t.Fatalf("repoFromAPIURL = %q", got)
	}
	if got := repoFromAPIURL("https://example.test/nope"); got != "" {
		t.Fatalf("repoFromAPIURL without /repos/ = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := collapse(" a \n b\t c "); got != "a b c" {
		t.Fatalf("collapse = %q", got)
	}
}

// TestToolDescriptionsLabelGitHubTextAsUntrusted keeps the Agent prompt honest:
// every GitHub result is untrusted facts, never instructions.
func TestToolDescriptionsLabelGitHubTextAsUntrusted(t *testing.T) {
	reg, _ := registerModule(t, testConfig(t, nil), fixedSecret("token"))
	for _, name := range []string{Name, PRListName, NotificationsName} {
		info, ok := reg.Info(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !strings.Contains(strings.ToLower(info.Description), "untrusted") {
			t.Errorf("%s description = %q, want the untrusted-facts label", name, info.Description)
		}
	}
}
