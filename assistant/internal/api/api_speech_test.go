package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/api"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// fakeSpeaker stands in for speech.Speaker at the reply session seam.
type fakeSpeaker struct {
	spoke  []string
	err    error
	ctxErr error
}

func (f *fakeSpeaker) SayText(ctx context.Context, text string) error {
	f.spoke = append(f.spoke, text)
	f.ctxErr = ctx.Err()
	return f.err
}

func newSpeechServer(t *testing.T, speaker reply.Speaker) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	session := reply.New(p, speaker, nil, nil)
	return api.NewHandler(p, logger, true, api.WithResponder(session)), &logs
}

func TestDebugTranscriptSpeaksTheReply(t *testing.T) {
	speaker := &fakeSpeaker{}
	handler, _ := newSpeechServer(t, speaker)

	rec := postTranscript(t, handler, `{"text":"what time is it"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Reply  string `json:"reply"`
		Spoken bool   `json:"spoken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if !body.Spoken {
		t.Fatal("spoken = false, want true")
	}
	if len(speaker.spoke) != 1 || speaker.spoke[0] != body.Reply {
		t.Fatalf("spoke = %v, want the reply %q spoken once", speaker.spoke, body.Reply)
	}
	if speaker.ctxErr != nil {
		t.Fatalf("speech context was cancelled: %v", speaker.ctxErr)
	}
}

func TestDebugTranscriptKeepsReplyWhenSpeechFails(t *testing.T) {
	speaker := &fakeSpeaker{err: errors.New("tts is down")}
	handler, logs := newSpeechServer(t, speaker)

	rec := postTranscript(t, handler, `{"text":"what time is it"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Reply  string `json:"reply"`
		Spoken bool   `json:"spoken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Reply == "" {
		t.Fatal("reply is empty")
	}
	if body.Spoken {
		t.Fatal("spoken = true, want false")
	}
	if strings.Contains(rec.Body.String(), "tts is down") {
		t.Fatalf("response leaked the internal error: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "tts is down") {
		t.Fatalf("failure was not logged: %s", logs.String())
	}
}

func TestDebugTranscriptWithoutSpeechReportsNotSpoken(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := postTranscript(t, handler, `{"text":"what time is it"}`)
	var body struct {
		Spoken bool `json:"spoken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Spoken {
		t.Fatal("spoken = true, want false without a speaker")
	}
}
