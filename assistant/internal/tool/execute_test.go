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

func TestPerToolTimeoutCancelsHandler(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.Timeout = 25 * time.Millisecond
	tl.Handler = func(ctx context.Context, _ echoIn) (echoOut, error) {
		<-ctx.Done()
		return echoOut{}, ctx.Err()
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	started := time.Now()
	_, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`))
	if err == nil {
		t.Fatal("Execute succeeded, want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s, want close to 25ms", elapsed)
	}
}

func TestRegistryDefaultTimeoutApplies(t *testing.T) {
	reg := tool.NewRegistry(25 * time.Millisecond)
	tl := echoTool()
	tl.Handler = func(ctx context.Context, _ echoIn) (echoOut, error) {
		<-ctx.Done()
		return echoOut{}, ctx.Err()
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
}

func TestTimeoutReportedEvenWhenHandlerIgnoresContext(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.Timeout = 10 * time.Millisecond
	tl.Handler = func(context.Context, echoIn) (echoOut, error) {
		time.Sleep(30 * time.Millisecond)
		return echoOut{Text: "late"}, nil
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
}

func TestToolErrorIsSpokenAsIs(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.Handler = func(context.Context, echoIn) (echoOut, error) {
		return echoOut{}, &tool.ToolError{Say: "The timer is already off."}
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`))
	if err == nil {
		t.Fatal("Execute succeeded, want error")
	}
	if got := tool.FailureMessage(err); got != "The timer is already off." {
		t.Fatalf("FailureMessage = %q", got)
	}
}

func TestInternalErrorSurfacesOnlyGenericFailure(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	tl := echoTool()
	tl.Handler = func(context.Context, echoIn) (echoOut, error) {
		return echoOut{}, errors.New("dial postgres://user:hunter2@db failed")
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := reg.Execute(context.Background(), "test.echo", json.RawMessage(`{"text":"hi"}`))
	if err == nil {
		t.Fatal("Execute succeeded, want error")
	}
	msg := tool.FailureMessage(err)
	if msg == "" {
		t.Fatal("FailureMessage is empty")
	}
	for _, leak := range []string{"postgres", "hunter2", "db failed"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("FailureMessage leaked %q: %q", leak, msg)
		}
	}
	if got := tool.FailureMessage(errors.New("boom")); got != tool.DefaultFailureMessage {
		t.Fatalf("FailureMessage = %q, want generic %q", got, tool.DefaultFailureMessage)
	}
}

func TestRequestContextReachesHandler(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	var got tool.Request
	tl := echoTool()
	tl.Handler = func(ctx context.Context, _ echoIn) (echoOut, error) {
		var ok bool
		got, ok = tool.RequestFrom(ctx)
		if !ok {
			return echoOut{}, errors.New("request context missing")
		}
		return echoOut{}, nil
	}
	if err := tool.Add(reg, tl); err != nil {
		t.Fatalf("Add: %v", err)
	}

	want := tool.Request{ID: "req-1", Path: tool.PathControlPhrase, Device: tool.DevicePi}
	ctx := tool.WithRequest(context.Background(), want)
	if _, err := reg.Execute(ctx, "test.echo", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != want {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
}
