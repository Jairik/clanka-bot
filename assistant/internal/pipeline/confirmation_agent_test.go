package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

const confirmationTestKeyEnv = "PIPELINE_CONFIRMATION_TEST_API_KEY"

// confirmToolCallReply scripts one OpenAI-compatible streamed tool-call turn.
func confirmToolCallReply(name, args string) string {
	var b strings.Builder
	chunk := map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "test",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "call-1",
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	fmt.Fprintf(&b, "data: %s\n\n", raw)
	fmt.Fprint(&b, `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestModelToolCallAsksThenYesRunsThroughTheRealAgentLoop covers the whole
// #24 path: fake model tool call -> registry pending -> spoken question -> yes.
func TestModelToolCallAsksThenYesRunsThroughTheRealAgentLoop(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	addSendTool(t, reg, &runs)

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, confirmToolCallReply("mail__send", `{"to":"doctor"}`))
	}))
	t.Cleanup(server.Close)

	t.Setenv(confirmationTestKeyEnv, "test-key")
	agentPath, err := agent.New(agent.Settings{
		BaseURL:            server.URL,
		APIKeyEnv:          confirmationTestKeyEnv,
		Model:              "test-model",
		UnavailableMessage: "unavailable",
		FirstTokenTimeout:  2 * time.Second,
	}, reg, nil)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(agentPath, nil),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	question := p.Handle(context.Background(), request(), "email my doctor")
	if question.Text != "Send the email to doctor?" {
		t.Fatalf("question = %q, want the registry summary", question.Text)
	}
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times before the yes", runs)
	}
	if requests != 1 {
		t.Fatalf("provider requests = %d, want the loop to stop at the pending call", requests)
	}
	if pending, ok := reg.Pending(); !ok || pending.Tool != "mail.send" {
		t.Fatalf("registry pending = %+v, %v, want mail.send", pending, ok)
	}

	reply := p.Handle(context.Background(), request(), "yes")
	if runs != 1 {
		t.Fatalf("confirm handler ran %d times after the yes, want 1", runs)
	}
	if reply.Text != "Email sent." || reply.Path != tool.PathConfirmation {
		t.Fatalf("reply = %q (%s), want the confirmed Tool rendering", reply.Text, reply.Path)
	}
	if requests != 1 {
		t.Errorf("provider requests = %d, want the yes to need no model call", requests)
	}
}
