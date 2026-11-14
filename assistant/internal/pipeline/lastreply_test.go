package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// TestLastReplyRecordsProvenance checks that the shared store sees the final
// reply, its Tool and its Local-only declaration, so a follow-up cannot forward
// a private result.
func TestLastReplyRecordsProvenance(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	fixed := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	private := tool.Tool[fakeIn, fakeOut]{
		Name:        "test.private",
		Description: "A Local-only test Tool.",
		Permission:  tool.PermissionRead,
		LocalOnly:   tool.LocalOnlyYes,
		Handler: func(context.Context, fakeIn) (fakeOut, error) {
			return fakeOut{Value: "the private answer"}, nil
		},
		Speak: func(out fakeOut) string { return out.Value },
	}
	if err := tool.Add(reg, private); err != nil {
		t.Fatalf("Add private: %v", err)
	}
	store := lastreply.New()
	logger, _ := testLogger()
	p, err := pipeline.New(reg, map[string]string{
		"what time is it":      clock.Name,
		"read the private one": "test.private",
	}, logger, pipeline.WithLastReply(store))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	reply := p.Handle(context.Background(), request(), "what time is it")
	if reply.LocalOnly {
		t.Fatal("the clock reply was marked Local-only")
	}
	last, ok := store.Last()
	if !ok {
		t.Fatal("the last reply was not recorded")
	}
	if last.Text != reply.Text || last.Tool != clock.Name || last.Path != string(tool.PathControlPhrase) || last.LocalOnly {
		t.Fatalf("last = %+v, reply = %+v", last, reply)
	}

	reply = p.Handle(context.Background(), request(), "read the private one")
	if !reply.LocalOnly {
		t.Fatal("the private reply did not carry its Local-only provenance")
	}
	last, ok = store.Last()
	if !ok || !last.LocalOnly || last.Tool != "test.private" {
		t.Fatalf("last = %+v, %v; want a Local-only private reply", last, ok)
	}
}

// TestLastReplyIsRecordedForTheAgentPath checks the other provenance branch: an
// Agent answer has no Tool call and is not Local-only.
func TestLastReplyIsRecordedForTheAgentPath(t *testing.T) {
	reg := tool.NewRegistry(time.Second)
	runner := newFakeRunner()
	runner.result.Text = "an agent answer"
	store := lastreply.New()
	logger, _ := testLogger()
	p, err := pipeline.New(reg, nil, logger,
		pipeline.WithAgent(runner, nil),
		pipeline.WithLastReply(store),
	)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	reply := p.Handle(context.Background(), request(), "an open question")
	if reply.Path != tool.PathAgent {
		t.Fatalf("reply = %+v, want the Agent path", reply)
	}
	last, ok := store.Last()
	if !ok || last.Text != "an agent answer" || last.LocalOnly || last.Tool != "" {
		t.Fatalf("last = %+v, %v; want the agent answer without a Tool", last, ok)
	}
}
