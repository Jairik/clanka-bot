package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
)

type sendIn struct {
	To string `json:"to"`
}

type sendOut struct {
	Status string `json:"status"`
}

// sendTool is the test-only confirm Tool issue #24 uses; no production Tool is
// confirm.
func sendTool(runs *int) tool.Tool[sendIn, sendOut] {
	return tool.Tool[sendIn, sendOut]{
		Name:        "mail.send",
		Description: "Send an email.",
		Permission:  tool.PermissionConfirm,
		LocalOnly:   tool.LocalOnlyNo,
		Summary:     func(in sendIn) string { return "Send the email to " + in.To + "?" },
		Handler: func(_ context.Context, _ sendIn) (sendOut, error) {
			*runs++
			return sendOut{Status: "sent"}, nil
		},
		Speak: func(out sendOut) string { return "Email " + out.Status + "." },
	}
}

func TestExecuteRefusesAConfirmToolUntilConfirmed(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	if err := tool.Add(reg, sendTool(&runs)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := reg.Execute(context.Background(), "mail.send", json.RawMessage(`{"to":"doctor"}`))
	if !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("error = %v, want ErrConfirmationRequired", err)
	}
	var required *tool.ConfirmationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error = %T, want *ConfirmationRequiredError", err)
	}
	if got, want := required.Pending.Summary, "Send the email to doctor?"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times, want 0 without a confirmation", runs)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("a bypass attempt armed a pending call, want none")
	}
}

func TestProposeThenExecutePendingRunsTheStoredCallOnce(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	if err := tool.Add(reg, sendTool(&runs)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	pending, err := reg.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got, want := pending.Summary, "Send the email to doctor?"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	stored, ok := reg.Pending()
	if !ok || stored.Tool != "mail.send" {
		t.Fatalf("pending = %+v, ok = %v, want mail.send", stored, ok)
	}

	result, done, err := reg.ExecutePending(context.Background())
	if err != nil {
		t.Fatalf("ExecutePending: %v", err)
	}
	if done.Tool != "mail.send" || runs != 1 {
		t.Fatalf("executed %q %d times, want mail.send once", done.Tool, runs)
	}
	if out, ok := result.(sendOut); !ok || out.Status != "sent" {
		t.Fatalf("result = %#v, want a sent email", result)
	}

	if _, ok := reg.Pending(); ok {
		t.Error("pending was not consumed by the confirmation")
	}
	if _, _, err := reg.ExecutePending(context.Background()); !errors.Is(err, tool.ErrNoPending) {
		t.Fatalf("second ExecutePending error = %v, want ErrNoPending", err)
	}
	if runs != 1 {
		t.Fatalf("confirm handler ran %d times, want one turn to run it once", runs)
	}
}

func TestProposeKeepsAtMostOnePendingCall(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	if err := tool.Add(reg, sendTool(&runs)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := reg.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`)); err != nil {
		t.Fatalf("first Propose: %v", err)
	}
	second, err := reg.Propose("mail.send", json.RawMessage(`{"to":"dentist"}`))
	if err != nil {
		t.Fatalf("second Propose: %v", err)
	}
	if !strings.Contains(second.Summary, "dentist") {
		t.Fatalf("pending summary = %q, want the newer call", second.Summary)
	}

	result, done, err := reg.ExecutePending(context.Background())
	if err != nil {
		t.Fatalf("ExecutePending: %v", err)
	}
	if done.Summary != second.Summary {
		t.Errorf("ran %q, want only the newest pending call %q", done.Summary, second.Summary)
	}
	if out, ok := result.(sendOut); !ok || out.Status != "sent" {
		t.Fatalf("result = %#v", result)
	}
	if runs != 1 {
		t.Fatalf("confirm handler ran %d times, want 1", runs)
	}
}

func TestDropPendingDiscardsTheWaitingCall(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	if err := tool.Add(reg, sendTool(&runs)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := reg.Propose("mail.send", json.RawMessage(`{"to":"doctor"}`)); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	dropped, ok := reg.DropPending()
	if !ok || dropped.Tool != "mail.send" {
		t.Fatalf("DropPending = %+v, %v, want mail.send", dropped, ok)
	}
	if _, ok := reg.Pending(); ok {
		t.Error("pending survived DropPending")
	}
	if _, _, err := reg.ExecutePending(context.Background()); !errors.Is(err, tool.ErrNoPending) {
		t.Fatalf("ExecutePending after drop error = %v, want ErrNoPending", err)
	}
	if runs != 0 {
		t.Fatalf("confirm handler ran %d times, want 0", runs)
	}
}

func TestProposeRejectsNonConfirmTools(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	if err := tool.Add(reg, echoTool()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := reg.Propose("test.echo", json.RawMessage(`{"text":"hi"}`)); err == nil {
		t.Fatal("Propose accepted a read Tool")
	}
	if _, ok := reg.Pending(); ok {
		t.Error("rejected proposal armed a pending call")
	}
}

func TestConfirmSummaryFallsBackToTheDescription(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runs := 0
	def := sendTool(&runs)
	def.Summary = nil
	if err := tool.Add(reg, def); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := reg.Execute(context.Background(), "mail.send", nil)
	var required *tool.ConfirmationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error = %v, want *ConfirmationRequiredError", err)
	}
	if got, want := required.Pending.Summary, "Send an email?"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
