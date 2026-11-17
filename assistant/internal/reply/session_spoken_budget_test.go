package reply_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// slowReplyServer streams six markdown sentences one chunk at a time and
// reports when the client cancels the request, which is what the spoken budget
// must do after the cut.
func slowReplyServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	cancelled := make(chan struct{})
	var once bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			"**First sentence.**\n",
			"- Second sentence with a list marker. \xf0\x9f\x98\x8a\n",
			"Third sentence.\n",
			"Fourth sentence.\n",
			"Fifth sentence.\n",
			"Sixth sentence.\n",
		}
		for _, chunk := range chunks {
			payload, err := json.Marshal(map[string]any{
				"id": "chatcmpl-budget", "object": "chat.completion.chunk", "created": 1, "model": "test",
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{"role": "assistant", "content": chunk},
					"finish_reason": nil,
				}},
			})
			if err != nil {
				t.Errorf("marshal chunk: %v", err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-time.After(40 * time.Millisecond):
			case <-r.Context().Done():
				if !once {
					once = true
					close(cancelled)
				}
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, cancelled
}

func TestAgentReplyIsSpokenCleanedAndWithinTheBudget(t *testing.T) {
	server, cancelled := slowReplyServer(t)
	t.Setenv("REPLY_BUDGET_KEY", "test-key")
	registry := tool.NewRegistry(time.Second)
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	agentPath, err := agent.New(agent.Settings{
		BaseURL:            server.URL,
		APIKeyEnv:          "REPLY_BUDGET_KEY",
		Model:              "test/model",
		UnavailableMessage: "Sorry.",
		FirstTokenTimeout:  2 * time.Second,
		RequestTimeout:     5 * time.Second,
		Limits:             agent.DefaultLimits(),
	}, registry, logger)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	p, err := pipeline.New(registry, nil, logger, pipeline.WithAgent(agentPath, []string{"ask the agent"}))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	speaker := newFakeSpeaker()
	session := reply.New(p, speaker, nil, nil)
	result, err := session.Ask(context.Background(), request(), "ask the agent for a list")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	want := "First sentence. Second sentence with a list marker."
	if result.Text != want {
		t.Fatalf("reply = %q, want %q", result.Text, want)
	}
	if got := strings.Join(speaker.phrases(), " "); got != want {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	for _, unwanted := range []string{"**", "\xf0\x9f\x98\x8a", "Third", "Fourth", "Fifth", "Sixth", "- "} {
		if strings.Contains(result.Text, unwanted) {
			t.Errorf("spoken text contains %q: %q", unwanted, result.Text)
		}
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider stream was not cancelled after the spoken budget was reached")
	}
}
