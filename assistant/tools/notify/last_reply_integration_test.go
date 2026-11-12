package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	core "github.com/Jairik/clanka-bot/assistant/internal/notify"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
	"github.com/Jairik/clanka-bot/assistant/tools/notify"
)

type privateIn struct{}

type privateOut struct {
	Value string `json:"value"`
}

// TestSendThatResolvesThePreviousReplyAndRefusesLocalOnly is the acceptance
// path across two real turns: the pipeline records the clock reply, the phone
// Tool sends exactly that text, then a Local-only reply is refused with no
// network request.
func TestSendThatResolvesThePreviousReplyAndRefusesLocalOnly(t *testing.T) {
	var requests atomic.Int32
	var bodiesMu = make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests.Add(1)
		bodiesMu <- body
		_, _ = io.WriteString(w, `{"id":"test"}`)
	}))
	defer server.Close()

	last := lastreply.New()
	notifyService, err := core.New(core.Options{
		Reader:  newMapReader(map[string]any{core.URLKey: server.URL, core.MissedAlertsKey: true}),
		Secrets: newMapSecrets(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:  core.DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}

	reg := tool.NewRegistry(5 * time.Second)
	fixed := time.Date(2026, 11, 12, 15, 4, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	private := tool.Tool[privateIn, privateOut]{
		Name:        "test.private",
		Description: "A Local-only test Tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, privateIn) (privateOut, error) {
			return privateOut{Value: "the private answer"}, nil
		},
		Speak: func(out privateOut) string { return out.Value },
	}
	if err := tool.Add(reg, private); err != nil {
		t.Fatalf("register private: %v", err)
	}
	if err := notify.NewModule().Register(reg, tool.Deps{Notify: notifyService, LastReply: last}); err != nil {
		t.Fatalf("register notify: %v", err)
	}
	p, err := pipeline.New(reg, map[string]string{
		"what time is it":       clock.Name,
		"read my private list":  "test.private",
		"send that to my phone": notify.Name,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), pipeline.WithLastReply(last))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), tool.Request{ID: "1", Device: tool.DevicePi}, "what time is it")
	if reply.Text != "It's 3:04 PM." {
		t.Fatalf("clock reply = %q", reply.Text)
	}
	reply = p.Handle(context.Background(), tool.Request{ID: "2", Device: tool.DevicePi}, "send that to my phone")
	if reply.Tool != notify.Name || reply.Text != "Sent to your phone." {
		t.Fatalf("phone reply = %+v", reply)
	}
	select {
	case body := <-bodiesMu:
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.Message != "It's 3:04 PM." {
			t.Fatalf("sent message = %q, want the clock reply", payload.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the phone Tool sent no request")
	}

	reply = p.Handle(context.Background(), tool.Request{ID: "3", Device: tool.DevicePi}, "read my private list")
	if !reply.LocalOnly || reply.Text != "the private answer" {
		t.Fatalf("private reply = %+v", reply)
	}
	reply = p.Handle(context.Background(), tool.Request{ID: "4", Device: tool.DevicePi}, "send that to my phone")
	if !strings.Contains(reply.Text, "local-only") {
		t.Fatalf("refusal = %q", reply.Text)
	}
	if strings.Contains(reply.Text, "the private answer") {
		t.Fatal("the refusal echoed the Local-only reply")
	}
	select {
	case body := <-bodiesMu:
		t.Fatalf("a Local-only reply was sent: %s", body)
	case <-time.After(100 * time.Millisecond):
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want one (the clock reply only)", requests.Load())
	}
}
