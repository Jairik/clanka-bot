package reply_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/lastreply"
	"github.com/Jairik/clanka-bot/assistant/internal/pipeline"
	"github.com/Jairik/clanka-bot/assistant/internal/reply"
	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/Jairik/clanka-bot/assistant/tools/assistant"
	"github.com/Jairik/clanka-bot/assistant/tools/clock"
)

// TestSessionRepeatsThePreviousReply crosses the public reply.Session seam:
// the second turn is spoken from the same last-reply store the pipeline filled,
// not by feeding the current transcript back through TTS.
func TestSessionRepeatsThePreviousReply(t *testing.T) {
	fixed := time.Date(2026, 11, 11, 15, 4, 0, 0, time.UTC)
	reg := tool.NewRegistry(time.Second)
	if err := clock.Register(reg, func() time.Time { return fixed }); err != nil {
		t.Fatalf("register clock: %v", err)
	}
	store := lastreply.New()
	if err := assistant.NewModule().Register(reg, tool.Deps{LastReply: store}); err != nil {
		t.Fatalf("register assistant: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p, err := pipeline.New(reg, map[string]string{
		"what time is it": clock.Name,
		"say that again":  assistant.RepeatName,
	}, logger, pipeline.WithLastReply(store))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	speaker := newFakeSpeaker()
	session := reply.New(p, speaker, nil, nil)
	if _, err := session.Ask(context.Background(), request(), "what time is it"); err != nil {
		t.Fatalf("first Ask: %v", err)
	}
	if _, err := session.Ask(context.Background(), request(), "say that again"); err != nil {
		t.Fatalf("repeat Ask: %v", err)
	}

	want := []string{"It's 3:04 PM.", "It's 3:04 PM."}
	if got := speaker.phrases(); !equal(got, want) {
		t.Fatalf("spoken = %q, want the same reply twice", got)
	}
}
