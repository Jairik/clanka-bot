package hermes_test

import (
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
		Settings: svc, LocalBaseURL: "http://ornith.test/v1", LocalModel: "ornith:9b", LocalKey: "local-key",
		OpenCodeKey: func() string { return cloudKey },
	}
	requests := make(chan map[string]string, 4)
	var localAttempts atomic.Int32
	release := make(chan struct{})
	announcements := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	firstRequest := map[string]string{"task": "First", "provider": "local", "model": "ornith:9b", "key": "local-key"}
	wantRequest(firstRequest)
	cloudKey = "cloud-key"
	if _, err := manager.SetRuntime(ctx, cloud); err != nil {
		t.Fatal(err)
	}
	close(release)
	// Even a busy-admission retry must use the original selection and key.
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
		{LocalBaseURL: "http://ornith.test/v1", LocalModel: "ornith:9b"},
		{LocalBaseURL: "http://ornith.test/v1", LocalKey: "local-key"},
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
		if got := manager.Handle(t.Context(), "Explicit task"); got != "Hermes needs its local endpoint, model, and key." {
			t.Fatal(got)
		}
	}
}
