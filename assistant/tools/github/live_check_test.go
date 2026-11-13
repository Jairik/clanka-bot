//go:build githublive

// Opt-in live check for the GitHub Tools. It is excluded from every normal
// build and test run (build tag `githublive`) and is only meant for a person or
// an agent who has the operator's real fine-grained token.
//
// It drives the REAL Tool code (Module.Register, Registry.Propose,
// Registry.ExecutePending) against api.github.com, so the Confirmation path and
// the real HTTP client are exercised end to end. The token is read from the
// environment or from a .env file and is never printed, logged or embedded in
// an error: the harness reports only booleans, timings and status codes.
//
// Run it with the deploy .env (reads only GITHUB_TOKEN):
//
//	cd assistant && go test -tags githublive -run TestLive -v ./tools/github/
//
// Bounds: at most one real issue per create test, and it closes that issue
// (with a comment) immediately. Every request goes through the Tool's own repo
// allowlist, so nothing outside Jairik/clanka-bot can be written by accident.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"gopkg.in/yaml.v3"
)

const (
	liveRepo    = "Jairik/clanka-bot"
	liveTitle   = "[live check] clanka-bot github.issue_create"
	liveBody    = "Automated live check for #68: this issue is created by the real github.issue_create Tool path and closed immediately by the harness."
	defaultEnv  = "/home/jj/repos/clanka-bot-deploy/.env"
	liveTimeout = 30 * time.Second
)

// liveToken resolves the fine-grained token. It never returns it to a log.
func liveToken(t *testing.T) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); value != "" {
		t.Logf("token: present from environment (%d chars, not printed)", len(value))
		return value
	}
	path := strings.TrimSpace(os.Getenv("GITHUB_LIVE_ENV"))
	if path == "" {
		path = defaultEnv
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (set GITHUB_TOKEN or GITHUB_LIVE_ENV)", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "GITHUB_TOKEN="); ok {
			value := strings.Trim(strings.TrimSpace(rest), `"'`)
			if value == "" {
				t.Fatalf("%s has an empty GITHUB_TOKEN", path)
			}
			t.Logf("token: present from %s (%d chars, not printed)", path, len(value))
			return value
		}
	}
	t.Fatalf("%s has no GITHUB_TOKEN line", path)
	return ""
}

// liveConfig mirrors the shipped tools.github section for the live repo.
func liveConfig(t *testing.T) *Module {
	t.Helper()
	values := map[string]any{
		"base_url":      defaultBaseURL,
		"timeout":       "4s",
		"default_repo":  liveRepo,
		"allowed_repos": []string{liveRepo},
		"results":       10,
		"max_results":   20,
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

// liveModule wires the module with the real token and a capturing logger.
func liveModule(t *testing.T, token string) (*Module, *tool.Registry, *logBuffer) {
	t.Helper()
	module := liveConfig(t)
	logs := &logBuffer{}
	reg := tool.NewRegistry(5 * time.Second)
	if err := module.Register(reg, tool.Deps{
		Secrets: secretFunc(func(name string) (string, bool) {
			switch name {
			case SecretName:
				return token, true
			case NotificationsSecretName:
				return "", false
			}
			return "", false
		}),
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}); err != nil {
		t.Fatalf("register github: %v", err)
	}
	return module, reg, logs
}

// TestLiveNetworkPhases measures the connection phases the Tool's client uses,
// so a slow DNS, dial or TLS handshake is visible instead of being hidden
// behind one status=0 log line.
func TestLiveNetworkPhases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	host := "api.github.com"

	started := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	t.Logf("dns lookup %s: %s err=%v addrs=%v", host, time.Since(started).Round(time.Millisecond), err, addrs)

	started = time.Now()
	conn, err := (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	t.Logf("tcp dial (4s budget): %s err=%v", time.Since(started).Round(time.Millisecond), err)
	if err == nil {
		_ = conn.Close()
	}

	started = time.Now()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://" + host + "/")
	t.Logf("unauthenticated GET /: %s err=%v status=%v", time.Since(started).Round(time.Millisecond), err, liveStatus(resp))
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// liveStatus reports resp.StatusCode, or 0.
func liveStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// TestLiveTokenAccess reports the fine-grained token's repository permissions
// and the scopes GitHub attributes to it. It sends read-only requests and
// prints no token: only status codes and the permissions block.
func TestLiveTokenAccess(t *testing.T) {
	token := liveToken(t)
	module, _, _ := liveModule(t, token)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	for _, path := range []string{"/user", "/repos/" + liveRepo, "/repos/" + liveRepo + "/issues?per_page=1"} {
		status, headers, message := liveRequest(t, ctx, module, token, http.MethodGet, path, "")
		t.Logf("GET %s: status=%d scopes=%q accepted=%q remaining=%q message=%q",
			path, status, headers.Get("X-OAuth-Scopes"), headers.Get("X-Accepted-GitHub-Permissions"),
			headers.Get("X-RateLimit-Remaining"), message)
		if status < 200 || status > 299 {
			t.Fatalf("GET %s = %d, want 2xx", path, status)
		}
	}
}

// TestLiveCreatePermission probes the issue-write permission with a POST whose
// title is intentionally invalid. GitHub checks the token's permission before
// it validates the body, so a token with issues=write answers 422 and a token
// without it answers 403. Either way this creates nothing.
func TestLiveCreatePermission(t *testing.T) {
	token := liveToken(t)
	module, _, _ := liveModule(t, token)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	status, headers, message := liveRequest(t, ctx, module, token, http.MethodPost,
		"/repos/"+liveRepo+"/issues", `{"title":""}`)
	t.Logf("POST with an empty title: status=%d accepted=%q message=%q",
		status, headers.Get("X-Accepted-GitHub-Permissions"), message)
	switch status {
	case http.StatusUnprocessableEntity:
		t.Logf("issues=write is granted")
	case http.StatusForbidden:
		t.Logf("BLOCKED: the fine-grained GITHUB_TOKEN has no Issues: write on %s; "+
			"grant it in the token settings, then re-run this harness", liveRepo)
	default:
		t.Fatalf("probe status = %d, want 422 (permitted) or 403 (refused)", status)
	}
}

// liveIssueWriteAllowed reports whether the token can create an issue, using
// the same zero-write probe as TestLiveCreatePermission.
func liveIssueWriteAllowed(t *testing.T, ctx context.Context, module *Module, token string) bool {
	t.Helper()
	status, _, _ := liveRequest(t, ctx, module, token, http.MethodPost,
		"/repos/"+liveRepo+"/issues", `{"title":""}`)
	return status == http.StatusUnprocessableEntity
}

// TestLiveGitHubIssueCreate is the #68 live check: confirm, create one real
// issue, close it immediately, and report whether a failed create landed
// anyway (the "unknown outcome" half of the Tool's contract). It skips with an
// explicit blocker when the configured token lacks Issues: write, because no
// code change can grant that permission.
func TestLiveGitHubIssueCreate(t *testing.T) {
	token := liveToken(t)
	module, reg, logs := liveModule(t, token)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	if !liveIssueWriteAllowed(t, ctx, module, token) {
		t.Skipf("BLOCKED: %s has no Issues: write permission on %s; nothing can be created until the owner grants it", SecretName, liveRepo)
	}

	started := time.Now()
	err := module.Health(ctx)
	t.Logf("Health (GET /user): %s err=%v", time.Since(started).Round(time.Millisecond), err)
	if err != nil {
		t.Fatalf("the fine-grained token cannot reach api.github.com: %v", err)
	}

	// Reads first, exactly like the voice flow that measured "status 0" once.
	started = time.Now()
	login, err := module.client.Login(ctx)
	t.Logf("Login: %s login=%q err=%v", time.Since(started).Round(time.Millisecond), login, err)
	started = time.Now()
	total, prs, prErr := module.client.reviewRequestedPRs(ctx, liveRepo, 10)
	t.Logf("reviewRequestedPRs: %s total=%d returned=%d err=%v", time.Since(started).Round(time.Millisecond), total, len(prs), prErr)

	// The real confirm path: Propose arms, ExecutePending runs the one POST.
	args, _ := json.Marshal(IssueCreateIn{Title: liveTitle, Body: liveBody})
	if _, err := reg.Execute(ctx, Name, args); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("Execute before a yes = %v, want ErrConfirmationRequired", err)
	}
	if _, err := reg.Propose(Name, args); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	started = time.Now()
	result, pending, createErr := reg.ExecutePending(ctx)
	elapsed := time.Since(started).Round(time.Millisecond)
	t.Logf("ExecutePending %s: %s err=%v", pending.Tool, elapsed, createErr)

	var number int
	var url string
	switch value := result.(type) {
	case IssueCreateOut:
		number, url = value.Number, value.URL
		t.Logf("created issue #%d %s", value.Number, value.URL)
	default:
		t.Logf("result type = %T", value)
	}

	if createErr != nil {
		// The POST may have landed even though the reply could not confirm it.
		// This is the exact question the ambiguous spoken line leaves open.
		if found, ok := liveFindOpenIssue(t, module, liveTitle); ok {
			number = found
			t.Logf("create reported an error, but the issue exists: #%d", found)
		} else {
			t.Logf("create reported an error and the repository has no matching issue: nothing was created")
		}
	}

	if number != 0 {
		liveCloseIssue(t, module, number)
	}

	// A failure log must never carry the token or the body.
	t.Logf("module logs:\n%s", logs.String())
	if strings.Contains(logs.String(), token) {
		t.Fatal("the module log contains the token")
	}
	if strings.Contains(logs.String(), liveBody) {
		t.Fatal("the module log contains the issue body")
	}
	if createErr != nil {
		t.Fatalf("live create failed: %v", createErr)
	}
	if url == "" {
		t.Fatal("created issue has no URL")
	}
}

// TestLiveCreateFailureIsDiagnosable runs the real Tool path and proves that a
// refused write now reports a definitive HTTP status in the local log instead
// of the old status=0. It creates nothing when GitHub refuses, and closes the
// issue when it succeeds.
func TestLiveCreateFailureIsDiagnosable(t *testing.T) {
	token := liveToken(t)
	module, reg, logs := liveModule(t, token)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	args, _ := json.Marshal(IssueCreateIn{Title: liveTitle, Body: liveBody})
	if _, err := reg.Propose(Name, args); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	started := time.Now()
	result, _, createErr := reg.ExecutePending(ctx)
	t.Logf("ExecutePending: %s result=%T err=%v", time.Since(started).Round(time.Millisecond), result, createErr)

	if created, ok := result.(IssueCreateOut); ok && created.Number != 0 {
		liveCloseIssue(t, module, created.Number)
		t.Logf("the token can create issues; the log must be a success line: %s", logs.String())
		return
	}
	if createErr == nil {
		t.Fatal("create returned neither an issue nor an error")
	}

	// The failure must be definitive or an honest transport class, never an
	// unexplained status=0 hiding a real HTTP response.
	line := logs.String()
	t.Logf("module logs:\n%s", line)
	if strings.Contains(line, "status=0") && !strings.Contains(line, "kind=dns") &&
		!strings.Contains(line, "kind=connect") && !strings.Contains(line, "kind=tls") &&
		!strings.Contains(line, "kind=other") && !strings.Contains(line, "kind=timeout") {
		t.Fatalf("log = %q, want a definitive status or a named transport kind", line)
	}
	if strings.Contains(line, "kind=http") && strings.Contains(line, "status=0") {
		t.Fatalf("log = %q, an HTTP failure must keep its status", line)
	}
	if strings.Contains(line, token) || strings.Contains(line, liveBody) {
		t.Fatalf("log = %q, want no token or body", line)
	}
}

// liveRequest sends one bounded request with the real token and returns the
// status, headers and the bounded GitHub message field.
func liveRequest(t *testing.T, ctx context.Context, module *Module, token, method, path, body string) (int, http.Header, string) {
	t.Helper()
	var payload io.Reader
	if body != "" {
		payload = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, defaultBaseURL+path, payload)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Accept", acceptJSON)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", userAgent)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	resp, err := module.client.http.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw := make([]byte, 800)
	n, _ := resp.Body.Read(raw)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Clone(), liveMessage(raw[:n])
}

// liveFindOpenIssue checks the recent issue list for the marker title, so an
// "unknown outcome" failure can be answered with evidence instead of a guess.
func liveFindOpenIssue(t *testing.T, module *Module, title string) (int, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
	}
	path := "/repos/" + liveRepo + "/issues"
	query := url.Values{"state": {"all"}, "per_page": {"30"}}
	if _, err := module.client.do(ctx, module.client.token, http.MethodGet, path, query, nil, &issues); err != nil {
		t.Logf("could not list issues: %v", err)
		return 0, false
	}
	for _, issue := range issues {
		if issue.Title == title {
			return issue.Number, true
		}
	}
	return 0, false
}

// liveCloseIssue comments and closes the live-check issue with two bounded
// requests, using the same token and client as the Tool.
func liveCloseIssue(t *testing.T, module *Module, number int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	comment := struct {
		Body string `json:"body"`
	}{Body: "Live check passed and closed automatically. See #68."}
	path := "/repos/" + liveRepo + "/issues/" + strconv.Itoa(number)
	if _, err := module.client.do(ctx, module.client.token, http.MethodPost, path+"/comments", nil, comment, nil); err != nil {
		t.Errorf("comment on #%d: %v", number, err)
	}
	closed := struct {
		State string `json:"state"`
	}{State: "closed"}
	if _, err := module.client.do(ctx, module.client.token, http.MethodPatch, path, nil, closed, nil); err != nil {
		t.Errorf("close #%d: %v", number, err)
		return
	}
	t.Logf("closed issue #%d with a comment", number)
}

// TestLiveMissingToken is the missing-PAT acceptance: with no GITHUB_TOKEN the
// Health check names the secret, the issue Tool says it plainly, and no request
// is attempted (the call fails before any HTTP work).
func TestLiveMissingToken(t *testing.T) {
	module := liveConfig(t)
	logs := &logBuffer{}
	reg := tool.NewRegistry(5 * time.Second)
	if err := module.Register(reg, tool.Deps{
		Secrets: secretFunc(func(string) (string, bool) { return "", false }),
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	}); err != nil {
		t.Fatalf("register github: %v", err)
	}

	if got := module.RequiredSecrets(); len(got) != 2 || got[0] != SecretName {
		t.Fatalf("RequiredSecrets = %v, want %s first", got, SecretName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	if err := module.Health(ctx); err == nil || !strings.Contains(err.Error(), SecretName) ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Health = %v, want a clear missing %s row", err, SecretName)
	}

	args, _ := json.Marshal(IssueCreateIn{Title: liveTitle})
	if _, err := reg.Propose(Name, args); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	started := time.Now()
	_, _, err := reg.ExecutePending(ctx)
	if err == nil {
		t.Fatal("create without a token succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing-token create took %s, want a refusal before any request", elapsed)
	}
	spoken := tool.FailureMessage(err)
	t.Logf("issue_create without a token: %q", spoken)
	if !strings.Contains(spoken, SecretName) || !strings.Contains(spoken, "isn't configured") {
		t.Fatalf("spoken = %q, want the clear missing-token line", spoken)
	}
	got := logs.String()
	if !strings.Contains(got, "kind=config") || strings.Contains(got, "kind=http") {
		t.Fatalf("log = %q, want a config refusal, not a request failure", got)
	}

	// The separate notifications row must stay not-configured rather than
	// failing the module: it uses its own optional secret.
	check := module.AuxHealth(ctx)
	if len(check) != 1 || check[0].State != tool.StatusNotConfigured {
		t.Fatalf("AuxHealth = %+v, want the separate notifications row not-configured", check)
	}
}

// liveMessage extracts only the bounded message field of a GitHub error body.
func liveMessage(raw []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if len(payload.Message) > 200 {
		return payload.Message[:200]
	}
	return payload.Message
}
