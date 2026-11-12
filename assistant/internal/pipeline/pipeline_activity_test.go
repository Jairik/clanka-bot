package pipeline_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

// TestActivityDetailCarriesTheLocalOnlyResult proves the display record comes
// from the same request that routes: Tool name, Local-only provenance and the
// spoken result are attached to the completion, never to the model.
func TestActivityDetailCarriesTheLocalOnlyResult(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := tool.NewRegistry(time.Second)
	if err := tool.Add(registry, tool.Tool[struct{}, string]{
		Name:        "secret.read",
		Description: "Read a local secret.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, struct{}) (string, error) {
			return "the launch code is 1234", nil
		},
		Speak: func(string) string { return "The launch code is 1234." },
	}); err != nil {
		t.Fatalf("register secret.read: %v", err)
	}
	p, err := pipeline.New(registry, map[string]string{"read my secret": "secret.read"}, logger)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	var completed []latency.Completed
	timeline := latency.New("req-secret", latency.SourceDashboard, latency.WithObserver(func(c latency.Completed) {
		completed = append(completed, c)
	}))
	ctx := latency.WithContext(context.Background(), timeline)
	reply := p.Handle(ctx, tool.Request{ID: "req-secret"}, "read my secret")
	latency.Complete(ctx, logger, latency.OutcomeNoAudio, nil)

	if reply.Text != "The launch code is 1234." || !reply.LocalOnly {
		t.Fatalf("reply = %+v", reply)
	}
	if len(completed) != 1 || completed[0].Detail == nil {
		t.Fatalf("completed = %+v", completed)
	}
	detail := completed[0].Detail
	if !detail.LocalOnly || detail.Reply != reply.Text {
		t.Fatalf("detail provenance = %+v", detail)
	}
	if len(detail.Calls) != 1 || detail.Calls[0].Name != "secret.read" || !detail.Calls[0].LocalOnly {
		t.Fatalf("detail calls = %+v", detail.Calls)
	}
	if detail.Calls[0].Result != "The launch code is 1234." {
		t.Fatalf("detail result = %q", detail.Calls[0].Result)
	}
}

// TestActivityDetailCarriesAConfirmedCall covers the second path that executes
// a confirm Tool: the typed yes is answered by the same Confirmation stage and
// the executed call appears with its result.
func TestActivityDetailCarriesAConfirmedCall(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := tool.NewRegistry(time.Second)
	ran := 0
	if err := tool.Add(registry, tool.Tool[struct{}, string]{
		Name:        "mail.send",
		Description: "Send the mail.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Handler: func(context.Context, struct{}) (string, error) {
			ran++
			return "sent", nil
		},
		Speak: func(string) string { return "Sent." },
	}); err != nil {
		t.Fatalf("register mail.send: %v", err)
	}
	p, err := pipeline.New(registry, nil, logger,
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(registry, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if _, err := registry.Propose("mail.send", nil); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	var completed []latency.Completed
	timeline := latency.New("req-yes", latency.SourceDashboard, latency.WithObserver(func(c latency.Completed) {
		completed = append(completed, c)
	}))
	ctx := latency.WithContext(context.Background(), timeline)
	reply := p.Handle(ctx, tool.Request{ID: "req-yes"}, "yes")
	latency.Complete(ctx, logger, latency.OutcomeNoAudio, nil)

	if ran != 1 || reply.Path != tool.PathConfirmation {
		t.Fatalf("ran = %d, reply = %+v", ran, reply)
	}
	if len(completed) != 1 || completed[0].Detail == nil || len(completed[0].Detail.Calls) != 1 {
		t.Fatalf("completed = %+v", completed)
	}
	call := completed[0].Detail.Calls[0]
	if call.Name != "mail.send" || call.LocalOnly || call.Result != "Sent." {
		t.Fatalf("call detail = %+v", call)
	}
}
