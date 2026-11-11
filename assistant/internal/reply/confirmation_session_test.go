package reply_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/agent"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// proposingRunner arms a confirm Tool the way the real Agent loop does.
type proposingRunner struct {
	reg     *tool.Registry
	history *agent.History
	calls   int
}

func (r *proposingRunner) Run(_ context.Context, _ tool.Request, utterance string, _ func(string)) agent.Result {
	r.calls++
	if utterance == "email my doctor" {
		pending, err := r.reg.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`))
		if err != nil {
			return agent.Result{Text: "could not propose"}
		}
		return agent.Result{Text: pending.Summary, Pending: &pending}
	}
	return agent.Result{Text: "I can answer that."}
}

func (r *proposingRunner) History() *agent.History { return r.history }

// confirmationSession builds the shared reply Session over a real pipeline and
// registry, so the Confirmation flow is exercised through the spoken seam.
func confirmationSession(t *testing.T) (*reply.Session, *fakeSpeaker, *proposingRunner, *tool.Registry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time {
		return time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	}); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	err := tool.Add(reg, tool.Tool[struct {
		To string `json:"to"`
	}, struct {
		Status string `json:"status"`
	}]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Summary: func(in struct {
			To string `json:"to"`
		}) string {
			return "Send the email to " + in.To + "?"
		},
		Handler: func(context.Context, struct {
			To string `json:"to"`
		}) (struct {
			Status string `json:"status"`
		}, error) {
			return struct {
				Status string `json:"status"`
			}{Status: "sent"}, nil
		},
		Speak: func(out struct {
			Status string `json:"status"`
		}) string {
			return "Email " + out.Status + "."
		},
	})
	if err != nil {
		t.Fatalf("register mail.send: %v", err)
	}

	runner := &proposingRunner{reg: reg, history: agent.NewHistory(10, 5*time.Minute, nil)}
	p, err := pipeline.New(reg, map[string]string{"what time is it": clock.Name}, logger,
		pipeline.WithAgent(runner, []string{"ask the agent"}),
		pipeline.WithConfirmationResolver(pipeline.NewConfirmation(reg, logger)),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	speaker := newFakeSpeaker()
	return reply.New(p, speaker, nil, nil), speaker, runner, reg
}

func TestConfirmationQuestionAndYesAreSpokenThroughTheSession(t *testing.T) {
	session, speaker, _, reg := confirmationSession(t)

	if _, err := session.Ask(context.Background(), request(), "email my doctor"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if pending, ok := reg.Pending(); !ok || pending.Tool != "mail.send" {
		t.Fatalf("pending = %+v, %v, want mail.send", pending, ok)
	}
	if _, err := session.Ask(context.Background(), request(), "yes"); err != nil {
		t.Fatalf("Ask yes: %v", err)
	}
	if got, want := speaker.phrases(), []string{"Send the email to doctor?", "Email sent."}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("pending survived the yes")
	}
}

func TestUnrelatedRequestSpeaksCancelledBeforeItsAnswer(t *testing.T) {
	session, speaker, _, _ := confirmationSession(t)

	if _, err := session.Ask(context.Background(), request(), "email my doctor"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.Ask(context.Background(), request(), "what time is it"); err != nil {
		t.Fatalf("Ask time: %v", err)
	}
	if got, want := speaker.phrases(), []string{"Send the email to doctor?", "Cancelled.", "It's 3:04 PM."}; !equal(got, want) {
		t.Fatalf("spoken = %q, want %q", got, want)
	}
}
