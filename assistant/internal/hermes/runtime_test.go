package hermes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/hermes"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
)

// TestRuntimeSelection observes the actual sidecar payload across live changes and restarts.
func TestRuntimeSelection(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := settings.New(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	var cloudKey string
	config := hermes.RuntimeConfig{
		Settings: svc, LocalBaseURL: "http://ornith.test/v1", LocalModel: "ornith:9b",
		OpenCodeKey: func() string { return cloudKey },
	}
	requests := make(chan map[string]string, 4)
	var localAttempts atomic.Int32
	release := make(chan struct{})
	announcements := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprint(w, `{"status":"ok","local_model":"ornith:9b","local_base_url":"http://ornith.test/v1","local_ready":true}`)
			return
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		requests <- input
		if input["task"] == "First" && localAttempts.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if input["task"] == "Second" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"text":"Done."}`)
	}))
	defer server.Close()
	token := strings.Repeat("t", 32)
	manager, err := hermes.New(ctx, st.DB(), server.URL, token, config, func(state string) {
		announcements <- state
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if view := manager.Runtime(); view.Provider != "local" || view.OpenCodeModel != "deepseek-v4.1-flash" || view.OpenCodeReady || !view.LocalReady {
		t.Fatalf("default selection = %+v", view)
	}
	local := hermes.Selection{Provider: "local", OpenCodeModel: "glm-5.3-flash"}
	cloud := hermes.Selection{Provider: "opencode-go", OpenCodeModel: "kimi-k3"}
	if _, err := manager.SetRuntime(ctx, local); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetRuntime(ctx, cloud); !errors.Is(err, hermes.ErrCloudKey) || manager.Runtime().Selection != local {
		t.Fatalf("cloud selection without a key = %v", err)
	}
	if got := manager.Handle(ctx, "First"); got != "Hermes is working on job 1." {
		t.Fatal(got)
	}
	wantRequest := func(want map[string]string) {
		t.Helper()
		select {
		case got := <-requests:
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("request = %v, want %v", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("sidecar request did not arrive")
		}
	}
	firstRequest := map[string]string{"task": "First", "provider": "local", "model": "ornith:9b", "key": ""}
	wantRequest(firstRequest)
	cloudKey = "cloud-key"
	if _, err := manager.SetRuntime(ctx, cloud); err != nil {
		t.Fatal(err)
	}
	close(release)
	// A busy-admission retry keeps the original selection and sends no local credential.
	wantRequest(firstRequest)
	select {
	case state := <-announcements:
		if state != "Hermes finished." {
			t.Fatal(state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("local job did not finish")
	}
	if got := manager.Handle(ctx, "Second"); got != "Hermes is working on job 2." {
		t.Fatal(got)
	}
	wantRequest(map[string]string{"task": "Second", "provider": "opencode-go", "model": "kimi-k3", "key": "cloud-key"})
	select {
	case state := <-announcements:
		if state != "Hermes failed." {
			t.Fatal(state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cloud job did not finish")
	}
	cloudKey = ""
	if got := manager.Handle(ctx, "Third"); got != "Hermes needs its OpenCode Go key." || manager.Configured() {
		t.Fatal(got)
	}
	select {
	case extra := <-requests:
		t.Fatalf("unexpected sidecar request = %v", extra)
	default:
	}
	manager.Close()
	reopened, err := hermes.New(ctx, st.DB(), server.URL, token, config, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Runtime().Selection; got != cloud {
		t.Fatalf("restored selection = %+v", got)
	}
}

// TestIncompleteLocalConfiguration ensures a ready cloud key cannot activate a default local job.
func TestIncompleteLocalConfiguration(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, config := range []hermes.RuntimeConfig{
		{LocalBaseURL: "http://ornith.test/v1"},
		{LocalBaseURL: "http://ornith.test/v1", LocalModel: "  "},
	} {
		config.OpenCodeKey = func() string { return "cloud-key" }
		manager, err := hermes.New(t.Context(), st.DB(), "", strings.Repeat("t", 32), config, nil, slog.Default())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(manager.Close)
		if view := manager.Runtime(); view.Provider != "local" || view.LocalReady || !view.OpenCodeReady {
			t.Fatalf("runtime view = %+v", view)
		}
		if got := manager.Handle(t.Context(), "Explicit task"); got != "Hermes needs its local endpoint and model." {
			t.Fatal(got)
		}
	}
}

// TestLocalReadinessFollowsTheSidecar covers a sidecar whose own state differs
// from the Assistant's: each failure gets its own message, Local isn't ready,
// and no job is sent. An older sidecar without the fields fails closed. The
// cloud selection never waits on the probe.
func TestLocalReadinessFollowsTheSidecar(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := settings.New(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	const match = `{"status":"ok","local_model":"ornith:9b","local_base_url":"http://ornith.test/v1","local_ready":true}`
	var health atomic.Value // func(http.ResponseWriter)
	var probes, runs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			probes.Add(1)
			health.Load().(func(http.ResponseWriter))(w)
			return
		}
		runs.Add(1)
		fmt.Fprint(w, `{"text":"Done."}`)
	}))
	defer server.Close()
	body := func(text string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { fmt.Fprint(w, text) }
	}
	config := hermes.RuntimeConfig{
		Settings: svc, LocalBaseURL: "http://ornith.test/v1", LocalModel: "ornith:9b",
		OpenCodeKey: func() string { return "cloud-key" },
	}
	manager, err := hermes.New(ctx, st.DB(), server.URL+"/run", strings.Repeat("t", 32), config, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	const unreachable = "isn't reachable"
	const different = "different local settings"
	for _, tc := range []struct {
		name   string
		health func(http.ResponseWriter)
		want   string // substring of the refusal
	}{
		{"cloud-mode sidecar", body(`{"status":"ok","local_model":"","local_base_url":"","local_ready":false}`), different},
		{"other model", body(strings.Replace(match, "ornith:9b", "other:7b", 1)), different},
		{"old endpoint", body(strings.Replace(match, "ornith.test", "old-host", 1)), different},
		{"missing local key", body(strings.Replace(match, "true", "false", 1)), "needs its local endpoint key"},
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, unreachable},
		{"redirect", func(w http.ResponseWriter) {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
		}, unreachable},
		{"not JSON", body("ok"), unreachable},
		{"oversized", body(`{"status":"ok","local_model":"ornith:9b","pad":"` + strings.Repeat("x", 2048) + `","local_base_url":"http://ornith.test/v1"}`), unreachable},
	} {
		health.Store(tc.health)
		if view := manager.Runtime(); view.Provider != "local" || view.LocalReady {
			t.Fatalf("%s: view = %+v", tc.name, view)
		}
		if manager.Configured() {
			t.Fatalf("%s: should not be configured", tc.name)
		}
		if got := manager.Handle(ctx, "Explicit task"); !strings.Contains(got, tc.want) {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if runs.Load() != 0 {
		t.Fatalf("sent %d jobs to a sidecar that can't take them", runs.Load())
	}

	// Older sidecars do not report that they supply their own local credential.
	for _, old := range []string{`{"status":"ok"}`, strings.Replace(match, `,"local_ready":true`, "", 1)} {
		health.Store(body(old))
		if view := manager.Runtime(); view.LocalReady || !strings.Contains(view.LocalProblem, "out of date") {
			t.Fatalf("older sidecar %s: view = %+v", old, view)
		}
		if got := manager.Handle(ctx, "Explicit task"); !strings.Contains(got, "out of date") {
			t.Fatal(got)
		}
	}
	if runs.Load() != 0 {
		t.Fatalf("sent %d jobs to an older sidecar", runs.Load())
	}

	health.Store(body(match))
	if !manager.Runtime().LocalReady || !manager.Configured() {
		t.Fatal("a matching sidecar should make Local ready")
	}

	// Status, result and cancel never probe.
	before := probes.Load()
	for _, command := range []string{"status", "result", "cancel", ""} {
		manager.Handle(ctx, command)
	}
	if probes.Load() != before {
		t.Fatal("status, result or cancel probed the sidecar")
	}

	// The cloud selection neither probes nor depends on the sidecar.
	if _, err := manager.SetRuntime(ctx, hermes.Selection{Provider: "opencode-go", OpenCodeModel: "kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	health.Store(func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	before = probes.Load()
	if !manager.Configured() {
		t.Fatal("cloud selection should stay configured")
	}
	if probes.Load() != before {
		t.Fatal("the cloud selection probed the sidecar")
	}
}

// TestUnreachableSidecarTimesOutWithoutHoldingTheLock checks the probe gives
// up and that status stays answerable while a probe hangs.
func TestUnreachableSidecarTimesOutWithoutHoldingTheLock(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	config := hermes.RuntimeConfig{LocalBaseURL: "http://ornith.test/v1", LocalModel: "ornith:9b"}
	manager, err := hermes.New(ctx, st.DB(), server.URL+"/run", strings.Repeat("t", 32), config, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	reply := make(chan string, 1)
	started := time.Now()
	go func() { reply <- manager.Handle(ctx, "Explicit task") }()
	time.Sleep(100 * time.Millisecond)
	statusDone := make(chan string, 1)
	go func() { statusDone <- manager.Handle(ctx, "status") }()
	select {
	case <-statusDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("status blocked behind a hanging probe")
	}
	select {
	case got := <-reply:
		if !strings.Contains(got, "isn't reachable") || time.Since(started) > 3*time.Second {
			t.Fatalf("got %q after %s", got, time.Since(started))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not time out")
	}

	// A cancelled request stops waiting on the probe.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := manager.Handle(cancelled, "Explicit task"); !strings.Contains(got, "isn't reachable") && !strings.Contains(got, "isn't accepting") {
		t.Fatal(got)
	}
}

// TestConfiguredDuringALocalJob keeps the Dashboard's readiness steady while a
// local job runs; only a new job skips the probe.
func TestConfiguredDuringALocalJob(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprint(w, `{"status":"ok","local_model":"ornith:9b","local_base_url":"http://ornith.test/v1","local_ready":true}`)
			return
		}
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, `{"text":"Done."}`)
	}))
	defer server.Close()
	config := hermes.RuntimeConfig{LocalBaseURL: "http://ornith.test/v1", LocalModel: "ornith:9b"}
	manager, err := hermes.New(ctx, st.DB(), server.URL+"/run", strings.Repeat("t", 32), config, func(string) {}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	defer close(release)
	if got := manager.Handle(ctx, "Long task"); got != "Hermes is working on job 1." {
		t.Fatal(got)
	}
	<-started
	if !manager.Configured() || !manager.Runtime().LocalReady {
		t.Fatal("Local should stay ready while a local job runs")
	}
	if got := manager.Handle(ctx, "Another task"); got != "Hermes is already working on job 1." {
		t.Fatal(got)
	}
}
