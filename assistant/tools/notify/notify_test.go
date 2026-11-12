package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	core "github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/quiet"
	"github.com/Jairik/clanka-bot/assistant/internal/secrets"
	"github.com/Jairik/clanka-bot/assistant/internal/settings"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/notify"
)

type mapReader struct {
	mu     sync.RWMutex
	values map[string]settings.Value
}

func newMapReader(values map[string]any) *mapReader {
	reader := &mapReader{values: make(map[string]settings.Value, len(values))}
	for key, value := range values {
		reader.values[key] = settings.Value{Key: key, Value: value}
	}
	return reader
}

func (r *mapReader) Get(key string) (settings.Value, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.values[key]
	return value, ok
}

type mapSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func newMapSecrets() *mapSecrets { return &mapSecrets{values: make(map[string]string)} }

func (s *mapSecrets) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[name]
	return value, ok
}

func (s *mapSecrets) Set(_ context.Context, name, value string) (secrets.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = strings.TrimSpace(value)
	return secrets.Metadata{Name: name, Set: true, Source: secrets.SourceStore}, nil
}

// quietAtNoon builds a real Quiet service whose window covers noon.
func quietAtNoon(t *testing.T) *quiet.Service {
	t.Helper()
	svc := quiet.New(quiet.Options{
		Reader: newMapReader(map[string]any{
			quiet.EnabledKey: true,
			quiet.StartKey:   "00:00",
			quiet.EndKey:     "23:59",
			quiet.VolumeKey:  35,
		}),
		Now: func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
	if !svc.IsQuiet() {
		t.Fatal("test Quiet service is not quiet at noon")
	}
	return svc
}

type sentRequest struct {
	method string
	body   []byte
}

func fakeNTFY(t *testing.T) (*httptest.Server, <-chan sentRequest) {
	t.Helper()
	requests := make(chan sentRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- sentRequest{method: r.Method, body: body}
		io.WriteString(w, `{"id":"test"}`)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func buildTool(t *testing.T, serverURL string, quietSvc *quiet.Service, last *lastreply.Store) tool.Tool[notify.In, notify.Out] {
	t.Helper()
	svc, err := core.New(core.Options{
		Reader:  newMapReader(map[string]any{core.URLKey: serverURL, core.MissedAlertsKey: true}),
		Secrets: newMapSecrets(),
		Quiet:   quietSvc,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:  core.DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	return notify.New(notify.DefaultConfig(), svc, last)
}

func callTool(t *testing.T, tl tool.Tool[notify.In, notify.Out], in notify.In) (notify.Out, error) {
	t.Helper()
	return tl.Handler(context.Background(), in)
}

func decodeMessage(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Topic   string `json:"topic"`
		Message string `json:"message"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, body)
	}
	if payload.Topic == "" {
		t.Fatal("payload has no topic")
	}
	return payload.Message
}

func TestSendThatUsesTheLastReply(t *testing.T) {
	server, requests := fakeNTFY(t)
	last := lastreply.New()
	last.Record(lastreply.Reply{Text: "It's 3:04 PM.", Tool: "time.now", Path: "fast_path"})
	tl := buildTool(t, server.URL, nil, last)

	out, err := callTool(t, tl, notify.In{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !out.Sent || out.Muted || out.Source != "last_reply" {
		t.Fatalf("out = %+v", out)
	}
	if got := tl.Speak(out); got != "Sent to your phone." {
		t.Fatalf("Speak = %q", got)
	}
	select {
	case req := <-requests:
		if req.method != http.MethodPost {
			t.Fatalf("method = %s", req.method)
		}
		if got := decodeMessage(t, req.body); got != "It's 3:04 PM." {
			t.Fatalf("message = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake ntfy server received no request")
	}
}

func TestExplicitMessageWinsOverTheLastReply(t *testing.T) {
	server, requests := fakeNTFY(t)
	last := lastreply.New()
	last.Record(lastreply.Reply{Text: "old reply"})
	tl := buildTool(t, server.URL, nil, last)

	out, err := callTool(t, tl, notify.In{Message: "buy milk"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out.Source != "message" || !out.Sent {
		t.Fatalf("out = %+v", out)
	}
	select {
	case req := <-requests:
		if got := decodeMessage(t, req.body); got != "buy milk" {
			t.Fatalf("message = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake ntfy server received no request")
	}
}

func TestNoLastReplyIsAUserFacingError(t *testing.T) {
	server, requests := fakeNTFY(t)
	tl := buildTool(t, server.URL, nil, lastreply.New())

	_, err := callTool(t, tl, notify.In{})
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("error = %v, want a ToolError", err)
	}
	if !strings.Contains(toolErr.Say, "recent reply") {
		t.Fatalf("Say = %q", toolErr.Say)
	}
	select {
	case req := <-requests:
		t.Fatalf("an empty last reply sent a request: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLocalOnlyLastReplyIsRefused(t *testing.T) {
	server, requests := fakeNTFY(t)
	last := lastreply.New()
	last.Record(lastreply.Reply{
		Text:      "the private timer list",
		Tool:      "timer.list",
		Path:      "fast_path",
		LocalOnly: true,
	})
	tl := buildTool(t, server.URL, nil, last)

	_, err := callTool(t, tl, notify.In{})
	var toolErr *tool.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("error = %v, want a ToolError", err)
	}
	if !strings.Contains(toolErr.Say, "local-only") {
		t.Fatalf("Say = %q", toolErr.Say)
	}
	if strings.Contains(toolErr.Say, "private timer list") {
		t.Fatal("the refusal echoed the Local-only reply")
	}
	select {
	case req := <-requests:
		t.Fatalf("a Local-only reply was sent: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestQuietHoursMutesAndSaysSo(t *testing.T) {
	server, requests := fakeNTFY(t)
	last := lastreply.New()
	last.Record(lastreply.Reply{Text: "hello"})
	tl := buildTool(t, server.URL, quietAtNoon(t), last)

	out, err := callTool(t, tl, notify.In{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !out.Muted || out.Sent || out.Reason != "quiet hours" {
		t.Fatalf("out = %+v", out)
	}
	if got := tl.Speak(out); got != "Notifications are muted during quiet hours." {
		t.Fatalf("Speak = %q", got)
	}
	select {
	case req := <-requests:
		t.Fatalf("a quiet Notification was sent: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestExtractPhone(t *testing.T) {
	tl := buildTool(t, "http://127.0.0.1:1", nil, lastreply.New())
	cases := []struct {
		text    string
		message string
		ok      bool
	}{
		{"send that to my phone", "", true},
		{"send this to my phone please", "", true},
		{"send it to my phone", "", true},
		{"send your last reply to my phone", "", true},
		{"text me that", "", true},
		{"notify my phone", "", true},
		{"ping my phone", "", true},
		{"send me a notification", "", true},
		{"send buy milk to my phone", "buy milk", true},
		{"notify my phone that the oven is on", "the oven is on", true},
		{"text me the door code", "the door code", true},
		{"send the answer to my phone", "", true},
		{"what's my phone number", "", false},
		{"call my phone", "", false},
		{"turn on notifications", "", false},
		{"send an email to Sam", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			in, ok := tl.Extract(tc.text)
			if ok != tc.ok {
				t.Fatalf("Extract(%q) ok = %v, want %v", tc.text, ok, tc.ok)
			}
			if ok && in.Message != tc.message {
				t.Fatalf("Extract(%q) message = %q, want %q", tc.text, in.Message, tc.message)
			}
		})
	}
}

func TestToolMetadata(t *testing.T) {
	tl := buildTool(t, "http://127.0.0.1:1", nil, lastreply.New())
	if tl.Name != notify.Name {
		t.Fatalf("name = %q", tl.Name)
	}
	if tl.Permission != tool.PermissionAct {
		t.Fatalf("permission = %q, want act", tl.Permission)
	}
	if tl.LocalOnly != tool.LocalOnlyNo {
		t.Fatalf("LocalOnly = %v, want LocalOnlyNo", tl.LocalOnly)
	}
	if len(tl.Examples) < 20 {
		t.Fatalf("Examples = %d, want at least 20", len(tl.Examples))
	}
	if tl.Speak == nil || tl.Extract == nil {
		t.Fatal("Speak or Extract is missing")
	}
}
