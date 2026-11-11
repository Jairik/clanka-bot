package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

type fakeIn struct{}

type fakeOut struct {
	Value string `json:"value"`
}

func fakeTool(name string, handler func(context.Context, fakeIn) (fakeOut, error)) tool.Tool[fakeIn, fakeOut] {
	return tool.Tool[fakeIn, fakeOut]{
		Name:        name,
		Description: "Fake tool for pipeline tests.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyNo,
		Handler:     handler,
		Speak:       func(out fakeOut) string { return out.Value },
	}
}

func newPipeline(t *testing.T, reg *tool.Registry, phrases map[string]string, logger *slog.Logger) *pipeline.Pipeline {
	t.Helper()
	p, err := pipeline.New(reg, phrases, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func request() tool.Request {
	return tool.Request{ID: "req-1", Device: tool.DevicePi}
}

func TestControlPhraseReachesTimeNow(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	logger, _ := testLogger()
	p := newPipeline(t, reg, map[string]string{"what time is it": clock.Name}, logger)

	reply := p.Handle(context.Background(), request(), "What time is it?")
	if want := "It's 3:04 PM."; reply.Text != want {
		t.Fatalf("reply = %q, want %q", reply.Text, want)
	}
	if reply.Tool != clock.Name {
		t.Errorf("tool = %q, want %q", reply.Tool, clock.Name)
	}
	if reply.Path != tool.PathControlPhrase {
		t.Errorf("path = %q, want %q", reply.Path, tool.PathControlPhrase)
	}
	if reply.RequestID != "req-1" {
		t.Errorf("request id = %q", reply.RequestID)
	}
}

func TestUnknownTranscriptFallsThrough(t *testing.T) {
	logger, _ := testLogger()
	p := newPipeline(t, tool.NewRegistry(time.Second), nil, logger)
	reply := p.Handle(context.Background(), request(), "book me a flight")
	if reply.Text != pipeline.PlaceholderReply {
		t.Fatalf("reply = %q, want placeholder", reply.Text)
	}
	if reply.Path != tool.PathFallthrough {
		t.Fatalf("path = %q, want fallthrough", reply.Path)
	}
}

func TestToolErrorIsSpokenAsIs(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.fail", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{}, &tool.ToolError{Say: "The timer is already off."}
	})
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	logger, _ := testLogger()
	p := newPipeline(t, reg, map[string]string{"stop timer": "test.fail"}, logger)

	reply := p.Handle(context.Background(), request(), "stop timer")
	if reply.Text != "The timer is already off." {
		t.Fatalf("reply = %q", reply.Text)
	}
}

func TestInternalErrorIsGenericButLoggedInFull(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.explode", func(context.Context, fakeIn) (fakeOut, error) {
		return fakeOut{}, errors.New("open /etc/clanka/secret.key: permission denied")
	})
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	logger, logs := testLogger()
	p := newPipeline(t, reg, map[string]string{"explode": "test.explode"}, logger)

	reply := p.Handle(context.Background(), request(), "explode")
	if reply.Text != tool.DefaultFailureMessage {
		t.Fatalf("reply = %q, want generic failure", reply.Text)
	}
	if strings.Contains(reply.Text, "secret.key") {
		t.Fatalf("reply leaked internal detail: %q", reply.Text)
	}
	if !strings.Contains(logs.String(), "secret.key") {
		t.Fatalf("internal error was not logged in full: %s", logs.String())
	}
	if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %s", len(lines), logs.String())
	}
}

func TestOneStructuredLogLinePerRequest(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	logger, logs := testLogger()
	p := newPipeline(t, reg, map[string]string{"what time is it": clock.Name}, logger)

	p.Handle(context.Background(), request(), "what time is it")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %s", len(lines), logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	for _, key := range []string{"request_id", "path", "tool", "transcript", "duration_ms"} {
		if _, ok := record[key]; !ok {
			t.Errorf("log line missing %q: %s", key, lines[0])
		}
	}
	if record["request_id"] != "req-1" || record["path"] != string(tool.PathControlPhrase) {
		t.Errorf("log fields = %v", record)
	}
}

func TestNewRejectsControlPhrasePointingAtUnknownTool(t *testing.T) {
	_, err := pipeline.New(tool.NewRegistry(time.Second), map[string]string{"hi": "test.missing"}, slog.Default())
	if err == nil {
		t.Fatal("New accepted unknown Tool, want error")
	}
}

func TestNewRejectsControlPhraseWithoutSpeak(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := fakeTool("test.silent", func(context.Context, fakeIn) (fakeOut, error) { return fakeOut{}, nil })
	tl.Speak = nil
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := pipeline.New(reg, map[string]string{"hi": "test.silent"}, slog.Default()); err == nil {
		t.Fatal("New accepted Tool without Speak, want error")
	}
}
