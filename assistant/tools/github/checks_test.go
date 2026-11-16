package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// checksFake serves the two reads a default-ref checks call makes: the repo's
// default branch and one page of check runs. Any other path fails the test, so
// an accidental extra API call cannot hide behind a permissive fake.
func checksFake(t *testing.T, runs string) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, runs)
		case r.URL.Path == "/repos/acme/widgets":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}}
}

func TestChecksSummarisesGreenFailurePendingAndNoRuns(t *testing.T) {
	tests := []struct {
		name     string
		runs     string
		state    string
		total    int
		failures string
		say      string
	}{
		{
			name:  "all green",
			runs:  `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"test","status":"completed","conclusion":"success"}]}`,
			state: checkStateSuccess, total: 2,
			say: "Checks passed for acme/widgets on main (2 runs).",
		},
		{
			name:  "one failure",
			runs:  `{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"unit tests","status":"completed","conclusion":"failure"}]}`,
			state: checkStateFailure, total: 2, failures: "unit tests",
			say: "Checks failed for acme/widgets on main: unit tests.",
		},
		{
			name:  "still pending beats a failure",
			runs:  `{"total_count":2,"check_runs":[{"name":"unit tests","status":"completed","conclusion":"failure"},{"name":"deploy","status":"in_progress"}]}`,
			state: checkStatePending, total: 2, failures: "unit tests",
			say: "Checks are still running for acme/widgets on main (2 runs).",
		},
		{
			name:  "no runs",
			runs:  `{"total_count":0,"check_runs":[]}`,
			state: checkStateNone, total: 0,
			say: "No check runs found for acme/widgets on main.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := checksFake(t, tt.runs)
			server := fake.start(t)
			reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

			result, err := reg.Execute(context.Background(), ChecksName, nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			out := result.(ChecksOut)
			if out.Repo != "acme/widgets" || out.Ref != "main" || out.State != tt.state || out.Total != tt.total || out.PR != 0 {
				t.Fatalf("out = %+v, want state=%s total=%d", out, tt.state, tt.total)
			}
			if got := strings.Join(out.Failures, ","); got != tt.failures {
				t.Fatalf("failures = %q, want %q", got, tt.failures)
			}
			if got := speakChecks(out); got != tt.say {
				t.Fatalf("spoken = %q, want %q", got, tt.say)
			}

			requests := fake.snapshot()
			if len(requests) != 2 {
				t.Fatalf("requests = %d, want 2 (default branch + one checks page)", len(requests))
			}
			checks := requests[1]
			if !strings.HasSuffix(checks.Path, "/repos/acme/widgets/commits/main/check-runs") {
				t.Fatalf("checks path = %q", checks.Path)
			}
			if got := checks.Query.Get("per_page"); got != "20" {
				t.Fatalf("per_page = %q, want 20", got)
			}
		})
	}
}

func TestChecksNamesUpToThreeTruncatedFailures(t *testing.T) {
	runs := `{"total_count":5,"check_runs":[
		{"name":"` + strings.Repeat("x", maxTitleRunes+10) + `","status":"completed","conclusion":"failure"},
		{"name":"second","status":"completed","conclusion":"timed_out"},
		{"name":"third","status":"completed","conclusion":"cancelled"},
		{"name":"fourth","status":"completed","conclusion":"failure"},
		{"name":"skipped one","status":"completed","conclusion":"skipped"}]}`
	fake := checksFake(t, runs)
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	result, err := reg.Execute(context.Background(), ChecksName, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(ChecksOut)
	if out.State != checkStateFailure || len(out.Failures) != maxNamedFailures {
		t.Fatalf("out = %+v, want failure with %d names", out, maxNamedFailures)
	}
	if got := []rune(out.Failures[0]); len(got) != maxTitleRunes+1 {
		t.Fatalf("first name runes = %d, want %d (truncated with an ellipsis)", len(got), maxTitleRunes+1)
	}
	if out.Failures[1] != "second" || out.Failures[2] != "third" {
		t.Fatalf("failures = %+v", out.Failures)
	}
}

func TestChecksUsesNamedRefWithoutLookingUpTheDefaultBranch(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	if _, err := reg.Execute(context.Background(), ChecksName, []byte(`{"ref":"release/1.2"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	requests := fake.snapshot()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1 (no default-branch lookup)", len(requests))
	}
	if !strings.Contains(requests[0].Path, "/commits/release/1.2/check-runs") {
		t.Fatalf("path = %q, want the named ref", requests[0].Path)
	}
}

func TestChecksResolvesMyLatestPRThroughSearch(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/issues":
			fmt.Fprint(w, `{"total_count":1,"items":[{"number":42}]}`)
		case "/repos/acme/widgets/pulls/42":
			fmt.Fprint(w, `{"number":42,"head":{"sha":"abc123"}}`)
		default:
			if strings.HasSuffix(r.URL.Path, "/check-runs") {
				fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
				return
			}
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	result, err := reg.Execute(context.Background(), ChecksName, []byte(`{"ref":"my latest PR"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := result.(ChecksOut)
	if out.PR != 42 || out.Ref != "abc123" || out.State != checkStateSuccess {
		t.Fatalf("out = %+v, want PR 42 pinned to abc123", out)
	}
	if got := speakChecks(out); !strings.Contains(got, "PR #42") {
		t.Fatalf("spoken = %q, want the PR number", got)
	}
	requests := fake.snapshot()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want search + pull + checks", len(requests))
	}
	if got := requests[0].Query.Get("q"); got != "is:open is:pr author:@me repo:acme/widgets" {
		t.Fatalf("search q = %q", got)
	}
	if got := requests[2].Query.Get("per_page"); got != "20" {
		t.Fatalf("checks per_page = %q", got)
	}
}

func TestChecksSaysNoOpenPRWhenTheSearchIsEmpty(t *testing.T) {
	fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	}}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	_, err := reg.Execute(context.Background(), ChecksName, []byte(`{"ref":"my latest PR"}`))
	if err == nil {
		t.Fatal("no open PR succeeded")
	}
	if got := tool.FailureMessage(err); !strings.Contains(got, "open pull request") {
		t.Fatalf("failure = %q", got)
	}
	if got := len(fake.snapshot()); got != 1 {
		t.Fatalf("requests = %d, want 1 (no check-runs call)", got)
	}
}

func TestChecksRefusesAnOutsideRepoBeforeAnyHTTP(t *testing.T) {
	fake := &fakeGitHub{}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

	_, err := reg.Execute(context.Background(), ChecksName, []byte(`{"repo":"evil/other"}`))
	if err == nil {
		t.Fatal("an outside repo succeeded")
	}
	if got := tool.FailureMessage(err); !strings.Contains(got, "not allowed") {
		t.Fatalf("failure = %q", got)
	}
	if got := len(fake.snapshot()); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestChecksMapsAuthRateLimitAndPermissionFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header map[string]string
		want   string
	}{
		{"unauthenticated", http.StatusUnauthorized, nil, "rejected the token"},
		{"rate limited", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "rate limit"},
		{"forbidden names the permission", http.StatusForbidden, nil, "Checks: read"},
		{"not found", http.StatusNotFound, nil, "couldn't find"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeGitHub{handler: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				for key, value := range tt.header {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.status)
			}}
			server := fake.start(t)
			reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret("token"))

			_, err := reg.Execute(context.Background(), ChecksName, nil)
			if err == nil {
				t.Fatal("request succeeded")
			}
			if got := tool.FailureMessage(err); !strings.Contains(got, tt.want) {
				t.Fatalf("failure = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChecksWithoutATokenSaysSoBeforeAnyHTTP(t *testing.T) {
	fake := &fakeGitHub{}
	server := fake.start(t)
	reg, _ := registerModule(t, testConfig(t, map[string]any{"base_url": server.URL}), fixedSecret(""))

	_, err := reg.Execute(context.Background(), ChecksName, nil)
	if err == nil {
		t.Fatal("checks succeeded without a token")
	}
	if got := tool.FailureMessage(err); !strings.Contains(got, "GITHUB_TOKEN") {
		t.Fatalf("failure = %q", got)
	}
	if got := len(fake.snapshot()); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}
