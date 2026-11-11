package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// stubAgent answers without a provider, so this test only covers the debug
// entry to Agent path wiring.
type stubAgent struct {
	history *agent.History
	answer  string
}

func (s *stubAgent) Run(context.Context, tool.Request, string, func(string)) agent.Result {
	return agent.Result{Text: s.answer}
}

func (s *stubAgent) History() *agent.History { return s.history }

func TestDebugTranscriptAnswersAnOpenQuestionThroughTheAgent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	runner := &stubAgent{history: agent.NewHistory(10, 5*time.Minute, nil), answer: "Because of Rayleigh scattering."}
	p, err := pipeline.New(tool.NewRegistry(time.Second), nil, logger, pipeline.WithAgent(runner, []string{"ask the agent"}))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	handler := api.NewHandler(p, logger, true)

	rec := postTranscript(t, handler, `{"text":"why is the sky blue"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reply string `json:"reply"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Reply != "Because of Rayleigh scattering." {
		t.Fatalf("reply = %q", body.Reply)
	}
	if body.Path != string(tool.PathAgent) {
		t.Fatalf("path = %q, want agent", body.Path)
	}
}
