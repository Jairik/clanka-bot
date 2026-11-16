package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// retryStep is one scripted provider response. A non-zero status is an HTTP
// error before any body; otherwise body is written as an SSE stream. abort
// closes the connection after the body, simulating a transport failure that
// happens after content was already streamed.
type retryStep struct {
	status int
	body   string
	delay  time.Duration
	abort  bool
}

// newRetryServer replies with the scripted steps in order, repeating the last
// one once the script is exhausted. It reports how many requests it received.
func newRetryServer(t *testing.T, steps ...retryStep) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		i := int(count.Add(1)) - 1
		step := steps[len(steps)-1]
		if i < len(steps) {
			step = steps[i]
		}
		if step.delay > 0 {
			select {
			case <-time.After(step.delay):
			case <-r.Context().Done():
				return
			}
		}
		if step.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(step.status)
			fmt.Fprint(w, `{"error":{"message":"scripted failure","type":"test"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, step.body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if step.abort {
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, &count
}

// contentOnlyReply writes one content chunk and no finish/[DONE], so the
// client sees a delta and then (with abort) a broken stream.
func contentOnlyReply(text string) string {
	var b strings.Builder
	chunk := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}
	writeChunk(&b, chunk)
	return b.String()
}

func runRetryTurn(t *testing.T, serverURL string, onDelta func(string)) agent.Result {
	t.Helper()
	a := newTestAgent(t, testSettings(serverURL), nil, nil)
	return a.Run(context.Background(), tool.Request{ID: "req-retry"}, "hello", onDelta)
}

// A transient 503 followed by a good stream yields the answer and one counted
// retry. This is the core of the fix (#292).
func TestTransientFailureIsRetriedOnce(t *testing.T) {
	server, count := newRetryServer(t,
		retryStep{status: http.StatusServiceUnavailable},
		retryStep{body: textReply("Recovered.")},
	)
	result := runRetryTurn(t, server.URL, nil)
	if result.Text != "Recovered." {
		t.Fatalf("reply = %q, want %q", result.Text, "Recovered.")
	}
	if result.Retries != 1 {
		t.Fatalf("Retries = %d, want 1", result.Retries)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want 2 (one failed, one retried)", got)
	}
	if result.Unavailable {
		t.Error("a successful retry left Unavailable true")
	}
}

// A stream that fails after a content delta must not be retried: the delta is
// already in the user's ears and repeating it would speak it twice.
func TestFailureAfterContentIsNotRetried(t *testing.T) {
	server, count := newRetryServer(t,
		retryStep{body: contentOnlyReply("Partial"), abort: true},
		retryStep{body: textReply("Recovered.")},
	)
	var deltas []string
	result := runRetryTurn(t, server.URL, func(delta string) { deltas = append(deltas, delta) })
	if got := count.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (no retry after content)", got)
	}
	if len(deltas) != 1 || deltas[0] != "Partial" {
		t.Fatalf("deltas = %q, want a single %q", deltas, "Partial")
	}
	if !result.Unavailable {
		t.Error("a terminal failure after content did not set Unavailable")
	}
	if result.Retries != 0 {
		t.Fatalf("Retries = %d, want 0", result.Retries)
	}
}

// A 4xx (auth/validation) will not change on an identical second request, so
// it is terminal.
func TestAuthErrorIsNotRetried(t *testing.T) {
	server, count := newRetryServer(t,
		retryStep{status: http.StatusUnauthorized},
		retryStep{body: textReply("Recovered.")},
	)
	result := runRetryTurn(t, server.URL, nil)
	if got := count.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (no retry on 401)", got)
	}
	if !result.Unavailable {
		t.Error("401 did not set Unavailable")
	}
	if result.Retries != 0 {
		t.Fatalf("Retries = %d, want 0", result.Retries)
	}
}

// A context cancellation is user intent (barge-in/shutdown), never a provider
// failure to retry.
func TestCancellationIsNotRetried(t *testing.T) {
	server, count := newRetryServer(t, retryStep{body: textReply("too late"), delay: 2 * time.Second})
	a := newTestAgent(t, testSettings(server.URL), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	defer cancel()
	result := a.Run(ctx, tool.Request{ID: "req-cancel-retry"}, "hello", nil)
	if got := count.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (no retry on cancellation)", got)
	}
	if !result.Cancelled {
		t.Fatalf("Cancelled = false for a cancelled turn: %+v", result)
	}
	if result.Retries != 0 {
		t.Fatalf("Retries = %d, want 0", result.Retries)
	}
}

// A first-token timeout already consumed its budget; retrying would make the
// user wait again.
func TestFirstTokenTimeoutIsNotRetried(t *testing.T) {
	server, count := newRetryServer(t, retryStep{body: textReply("too late"), delay: 2 * time.Second})
	settings := testSettings(server.URL)
	settings.FirstTokenTimeout = 100 * time.Millisecond
	a := newTestAgent(t, settings, nil, nil)
	result := a.Run(context.Background(), tool.Request{ID: "req-timeout-retry"}, "hello", nil)
	if got := count.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1 (no retry on first-token timeout)", got)
	}
	if !result.TimedOut {
		t.Fatalf("TimedOut = false for a first-token timeout: %+v", result)
	}
	if result.Retries != 0 {
		t.Fatalf("Retries = %d, want 0", result.Retries)
	}
}

// Both attempts failing still speaks the configured Unavailable line, with
// the one retry counted and no duplicate deltas.
func TestTwoFailuresStillSpeakUnavailable(t *testing.T) {
	server, count := newRetryServer(t,
		retryStep{status: http.StatusServiceUnavailable},
		retryStep{status: http.StatusBadGateway},
	)
	result := runRetryTurn(t, server.URL, nil)
	if result.Text != "Sorry, I can't answer that right now." {
		t.Fatalf("reply = %q, want the configured Unavailable message", result.Text)
	}
	if !result.Unavailable {
		t.Error("two failures did not set Unavailable")
	}
	if result.Retries != 1 {
		t.Fatalf("Retries = %d, want 1", result.Retries)
	}
	if got := count.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want 2", got)
	}
}
