package hermes_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/hermes"
	"github.com/Jairik/clanka-bot/assistant/internal/store"
	"log/slog"
)

func TestJobsFinishCancelAndRecover(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "assistant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token := strings.Repeat("t", 32)
	cancelStarted := make(chan struct{})
	cancelReachedSidecar := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]string
		if r.Header.Get("Authorization") != "Bearer "+token ||
			json.NewDecoder(r.Body).Decode(&input) != nil || input["key"] != "test-key" || len(input) != 2 {
			t.Error("sidecar received an unexpected request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if input["task"] == "Wait to be cancelled" {
			close(cancelStarted)
			select {
			case <-r.Context().Done():
				close(cancelReachedSidecar)
			case <-time.After(3 * time.Second):
				t.Error("sidecar did not observe cancellation")
			}
			return
		}
		if input["task"] != "Explain HTTP" {
			t.Error("sidecar did not receive the original text")
		}
		fmt.Fprint(w, `{"text":"A network protocol."}`)
	}))
	defer server.Close()

	announcement := make(chan string, 1)
	m, err := hermes.New(t.Context(), st.DB(), server.URL, token, func() string { return "test-key" },
		func(text string) { announcement <- text }, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if got := m.Handle(t.Context(), "Explain HTTP"); got != "Hermes is working on job 1." {
		t.Fatal(got)
	}
	select {
	case got := <-announcement:
		if got != "Hermes finished." {
			t.Fatal(got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job did not finish")
	}
	if got := m.Handle(t.Context(), "result"); got != "A network protocol." {
		t.Fatal(got)
	}
	if got := m.Handle(t.Context(), "Wait to be cancelled"); got != "Hermes is working on job 2." {
		t.Fatal(got)
	}
	select {
	case <-cancelStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("second job did not start")
	}
	if got := m.Handle(t.Context(), "cancel"); got != "Hermes job 2 was cancelled." {
		t.Fatal(got)
	}
	select {
	case <-cancelReachedSidecar:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not close the model request")
	}
	m.Close()

	// A restart marks work interrupted and does not replay it.
	if _, err := st.DB().ExecContext(t.Context(), "INSERT INTO hermes_jobs (state) VALUES ('running')"); err != nil {
		t.Fatal(err)
	}
	reopened, err := hermes.New(t.Context(), st.DB(), server.URL, "", nil, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Handle(t.Context(), "status"); got != "Hermes job 3 is interrupted." {
		t.Fatal(got)
	}
	if got := reopened.Handle(t.Context(), "Explain HTTP"); got != "Hermes is disabled." {
		t.Fatal(got)
	}
}
